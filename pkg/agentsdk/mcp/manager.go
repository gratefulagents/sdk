package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultConnectTimeout = 15 * time.Second
	clientName            = "gratefulagents"
	clientVersion         = "0.1.0"
)

// ToolDescriptor describes an MCP tool exposed to the LLM.
type ToolDescriptor struct {
	QualifiedName string
	ServerName    string
	ToolName      string
	Description   string
	// DisplayDescription is the sanitized, length-bounded, provenance-tagged
	// rendering of Description for use in approval UI text. Use this — never
	// the raw Description — anywhere an MCP-supplied string is shown to a
	// human operator confirming a sensitive action.
	DisplayDescription string
	// DisplayTitle is the sanitized, provenance-tagged tool title for the
	// approval UI.
	DisplayTitle string
	InputSchema  json.RawMessage
	ReadOnly     bool
}

// ResourceDescriptor describes an MCP resource entry.
type ResourceDescriptor struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	MIMEType    string `json:"mimeType,omitempty"`
	Description string `json:"description,omitempty"`
	Server      string `json:"server"`
}

type serverConn struct {
	name         string
	client       *mcpsdk.Client
	session      *mcpsdk.ClientSession
	capabilities *mcpsdk.ServerCapabilities
	// cmd is the underlying child process; retained so Close can guarantee
	// termination even if the SDK's graceful shutdown stalls. Nil when the
	// transport did not expose an exec.Cmd (e.g. injected for tests).
	cmd *exec.Cmd
}

// Manager holds connected MCP sessions and their exposed tools/resources.
type Manager struct {
	mu sync.RWMutex
	// key: original server name from config.
	servers map[string]*serverConn
	// sorted by QualifiedName.
	toolDescriptors []ToolDescriptor
	// key: QualifiedName.
	toolByQualifiedName map[string]ToolDescriptor
	// snapshot pins the .mcp.json path & content hash for the run; it is
	// only set when the manager was constructed via NewManager. Silent
	// reloads are refused — see ConfigSnapshot.VerifyUnchanged.
	snapshot ConfigSnapshot
}

// ConfigSnapshot returns the pinned snapshot of .mcp.json, if any. Callers
// should treat this as immutable — use snapshot.VerifyUnchanged to detect
// in-run mutation; do NOT silently reload.
func (m *Manager) ConfigSnapshot() ConfigSnapshot {
	if m == nil {
		return ConfigSnapshot{}
	}
	return m.snapshot
}

type managerOptions struct {
	permissionMode policy.PermissionMode
	executor       sandbox.Executor
}

// ManagerOption configures MCP subprocess execution.
type ManagerOption func(*managerOptions)

// WithPermissionMode sets the filesystem mode used for MCP stdio subprocesses.
func WithPermissionMode(mode policy.PermissionMode) ManagerOption {
	return func(opts *managerOptions) {
		opts.permissionMode = policy.NormalizePermissionMode(string(mode))
	}
}

// WithCommandExecutor sets the command executor used for MCP stdio subprocesses.
func WithCommandExecutor(executor sandbox.Executor) ManagerOption {
	return func(opts *managerOptions) {
		opts.executor = executor
	}
}

// NewManager reads .mcp.json from workDir and connects supported servers.
//
// It returns (nil, nil) when no config file exists or no servers are configured.
// When some servers fail but others connect, a non-nil Manager is returned along
// with a non-nil warning error.
func NewManager(ctx context.Context, workDir string, opts ...ManagerOption) (*Manager, error) {
	cfgPath := ConfigPathForWorkDir(workDir)
	snap, err := LoadConfigSnapshotInWorkspace(cfgPath, workDir)
	if err != nil {
		return nil, err
	}
	if len(snap.ContentSHA256) == 0 || len(snap.Config.MCPServers) == 0 {
		return nil, nil
	}

	m, setupErr := NewManagerFromConfig(ctx, workDir, snap.Config, opts...)
	if m != nil {
		m.snapshot = snap
	}
	if m == nil || len(m.servers) == 0 {
		if setupErr != nil {
			return nil, setupErr
		}
		return nil, nil
	}
	return m, setupErr
}

// NewManagerFromConfig creates a manager from the provided config.
func NewManagerFromConfig(ctx context.Context, workDir string, cfg Config, opts ...ManagerOption) (*Manager, error) {
	options := resolveManagerOptions(opts...)
	m := &Manager{
		servers:             make(map[string]*serverConn),
		toolByQualifiedName: make(map[string]ToolDescriptor),
	}

	var errs []error
	usedToolNames := make(map[string]struct{})

	serverNames := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	for _, serverName := range serverNames {
		srvCfg := cfg.MCPServers[serverName]
		if srvCfg.Enabled != nil && !*srvCfg.Enabled {
			continue
		}

		transportType := strings.TrimSpace(srvCfg.Type)
		if transportType != "" && !strings.EqualFold(transportType, "stdio") {
			errs = append(errs, fmt.Errorf("MCP server %q uses unsupported type %q (only stdio is supported)", serverName, srvCfg.Type))
			continue
		}

		conn, err := connectStdioServer(ctx, workDir, serverName, srvCfg, options)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.servers[serverName] = conn

		tools, err := listAllTools(ctx, conn.session)
		if err != nil {
			errs = append(errs, fmt.Errorf("MCP server %q: list tools: %w", serverName, err))
			continue
		}

		for _, tool := range tools {
			if tool == nil {
				continue
			}
			if !serverToolAllowed(srvCfg, tool.Name) {
				continue
			}

			qualifiedName := BuildToolName(serverName, tool.Name)
			qualifiedName = EnsureUniqueToolName(qualifiedName, usedToolNames)

			desc := ToolDescriptor{
				QualifiedName:      qualifiedName,
				ServerName:         serverName,
				ToolName:           tool.Name,
				Description:        strings.TrimSpace(tool.Description),
				DisplayDescription: SanitizeMCPDisplay(serverName, tool.Description),
				DisplayTitle:       SanitizeMCPDisplay(serverName, mcpToolTitle(tool)),
				InputSchema:        normalizeInputSchema(tool.InputSchema),
				ReadOnly:           trustedMCPReadOnly(srvCfg, tool),
			}
			m.toolDescriptors = append(m.toolDescriptors, desc)
			m.toolByQualifiedName[qualifiedName] = desc
		}
	}

	sort.Slice(m.toolDescriptors, func(i, j int) bool {
		return m.toolDescriptors[i].QualifiedName < m.toolDescriptors[j].QualifiedName
	})

	return m, errors.Join(errs...)
}

func trustedMCPReadOnly(cfg ServerConfig, tool *mcpsdk.Tool) bool {
	return cfg.TrustReadOnlyHint && tool != nil && tool.Annotations != nil && tool.Annotations.ReadOnlyHint
}

func serverToolAllowed(cfg ServerConfig, toolName string) bool {
	if len(cfg.AllowedTools) == 0 {
		return true
	}
	toolName = strings.TrimSpace(toolName)
	for _, allowed := range cfg.AllowedTools {
		if strings.TrimSpace(allowed) == toolName {
			return true
		}
	}
	return false
}

func mcpToolTitle(tool *mcpsdk.Tool) string {
	if tool == nil {
		return ""
	}
	if t := strings.TrimSpace(tool.Title); t != "" {
		return t
	}
	if tool.Annotations != nil {
		if t := strings.TrimSpace(tool.Annotations.Title); t != "" {
			return t
		}
	}
	return tool.Name
}

// filteredEnv returns cfg.Env with credential-bearing names stripped.
// Blocked names are logged via the standard library logger so operators can
// audit which secrets the .mcp.json attempted to pass through.
func filteredEnv(serverName string, cfg ServerConfig) map[string]string {
	out, blocked := FilterCredentialEnv(cfg.Env, cfg.AllowEnv)
	if len(blocked) > 0 {
		sort.Strings(blocked)
		log.Printf("mcp: server %q .mcp.json env stripped credential-bearing keys: %s (allow via allowEnv to opt in)",
			serverName, strings.Join(blocked, ", "))
	}
	return out
}

func resolveManagerOptions(opts ...ManagerOption) managerOptions {
	options := managerOptions{
		permissionMode: policy.PermissionModeWorkspaceWrite,
		executor:       sandbox.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.executor == nil {
		options.executor = sandbox.Default()
	}
	options.permissionMode = policy.NormalizePermissionMode(string(options.permissionMode))
	return options
}

// Close closes all active MCP sessions.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	servers := make([]*serverConn, 0, len(m.servers))
	for _, conn := range m.servers {
		servers = append(servers, conn)
	}
	m.servers = map[string]*serverConn{}
	m.toolDescriptors = nil
	m.toolByQualifiedName = map[string]ToolDescriptor{}
	m.mu.Unlock()

	var errs []error
	for _, conn := range servers {
		if conn == nil {
			continue
		}
		if conn.session != nil {
			if err := conn.session.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing MCP server %q: %w", conn.name, err))
			}
		}
		// Belt-and-suspenders: even if session.Close already reaped the
		// child, terminateProcess is safe to call. If the SDK stalled
		// (unresponsive child, broken pipes, ctx cancel mid-handshake),
		// this guarantees the process group is killed and waited.
		if err := terminateProcess(conn.cmd, 2*time.Second); err != nil {
			errs = append(errs, fmt.Errorf("terminating MCP server %q child: %w", conn.name, err))
		}
	}
	return errors.Join(errs...)
}

// ConnectedServerNames returns connected server names sorted alphabetically.
func (m *Manager) ConnectedServerNames() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolDescriptors returns all MCP-backed dynamic tool descriptors.
func (m *Manager) ToolDescriptors() []ToolDescriptor {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]ToolDescriptor, len(m.toolDescriptors))
	copy(out, m.toolDescriptors)
	return out
}

// HasResources reports whether any connected server exposes resources.
func (m *Manager) HasResources() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, conn := range m.servers {
		if conn != nil && conn.capabilities != nil && conn.capabilities.Resources != nil {
			return true
		}
	}
	return false
}

// CallTool calls a dynamic MCP tool by its qualified name.
func (m *Manager) CallTool(ctx context.Context, qualifiedName string, args map[string]any) (*mcpsdk.CallToolResult, error) {
	if m == nil {
		return nil, fmt.Errorf("MCP manager is not initialized")
	}

	m.mu.RLock()
	desc, ok := m.toolByQualifiedName[qualifiedName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown MCP tool %q", qualifiedName)
	}

	conn, err := m.getServer(desc.ServerName)
	if err != nil {
		return nil, err
	}

	result, err := conn.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      desc.ToolName,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("MCP %s/%s: %w", desc.ServerName, desc.ToolName, err)
	}
	return result, nil
}

// ListResources returns available resources for all servers or a specific server.
func (m *Manager) ListResources(ctx context.Context, serverName string) ([]ResourceDescriptor, error) {
	if m == nil {
		return nil, fmt.Errorf("MCP manager is not initialized")
	}

	targets, err := m.resourceServers(serverName)
	if err != nil {
		return nil, err
	}

	var (
		out  []ResourceDescriptor
		errs []error
	)
	for _, conn := range targets {
		resources, err := listAllResources(ctx, conn.session)
		if err != nil {
			errs = append(errs, fmt.Errorf("MCP server %q: list resources: %w", conn.name, err))
			continue
		}
		for _, resource := range resources {
			if resource == nil {
				continue
			}
			out = append(out, ResourceDescriptor{
				URI:         resource.URI,
				Name:        resource.Name,
				MIMEType:    resource.MIMEType,
				Description: resource.Description,
				Server:      conn.name,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Server == out[j].Server {
			return out[i].URI < out[j].URI
		}
		return out[i].Server < out[j].Server
	})

	if len(out) > 0 {
		return out, nil
	}
	return nil, errors.Join(errs...)
}

// ReadResource reads a specific resource from a specific server.
func (m *Manager) ReadResource(ctx context.Context, serverName, uri string) (*mcpsdk.ReadResourceResult, error) {
	if m == nil {
		return nil, fmt.Errorf("MCP manager is not initialized")
	}
	if strings.TrimSpace(uri) == "" {
		return nil, fmt.Errorf("resource URI is required")
	}

	conn, err := m.getServer(serverName)
	if err != nil {
		return nil, err
	}
	if conn.capabilities == nil || conn.capabilities.Resources == nil {
		return nil, fmt.Errorf("MCP server %q does not support resources", serverName)
	}

	result, err := conn.session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, fmt.Errorf("MCP server %q read %q: %w", serverName, uri, err)
	}
	return result, nil
}

func (m *Manager) getServer(name string) (*serverConn, error) {
	m.mu.RLock()
	conn, ok := m.servers[name]
	names := make([]string, 0, len(m.servers))
	for serverName := range m.servers {
		names = append(names, serverName)
	}
	m.mu.RUnlock()

	if ok {
		return conn, nil
	}

	sort.Strings(names)
	return nil, fmt.Errorf("MCP server %q not found (available: %s)", name, strings.Join(names, ", "))
}

func (m *Manager) resourceServers(serverName string) ([]*serverConn, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if serverName != "" {
		conn, ok := m.servers[serverName]
		if !ok {
			names := make([]string, 0, len(m.servers))
			for n := range m.servers {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("MCP server %q not found (available: %s)", serverName, strings.Join(names, ", "))
		}
		if conn.capabilities == nil || conn.capabilities.Resources == nil {
			return nil, fmt.Errorf("MCP server %q does not support resources", serverName)
		}
		return []*serverConn{conn}, nil
	}

	servers := make([]*serverConn, 0, len(m.servers))
	for _, conn := range m.servers {
		if conn != nil && conn.capabilities != nil && conn.capabilities.Resources != nil {
			servers = append(servers, conn)
		}
	}
	return servers, nil
}

func connectStdioServer(ctx context.Context, workDir, name string, cfg ServerConfig, opts managerOptions) (*serverConn, error) {
	safeEnv := sandbox.SafeEnvMap()
	command := strings.TrimSpace(sandbox.ExpandSafe(cfg.Command, safeEnv))
	if command == "" {
		return nil, fmt.Errorf("MCP server %q: command is required", name)
	}

	args := make([]string, len(cfg.Args))
	for i, arg := range cfg.Args {
		args[i] = sandbox.ExpandSafe(arg, safeEnv)
	}

	connectCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		connectCtx, cancel = context.WithTimeout(ctx, defaultConnectTimeout)
	}
	defer cancel()

	cmd, err := opts.executor.Build(connectCtx, sandbox.Request{
		Argv:           append([]string{command}, args...),
		WorkDir:        workDir,
		PermissionMode: opts.permissionMode,
		Env:            filteredEnv(name, cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("MCP server %q sandbox: %w", name, err)
	}
	configureProcessGroup(cmd)

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    clientName,
		Version: clientVersion,
	}, nil)

	session, err := client.Connect(connectCtx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("MCP server %q connect: %w", name, err)
	}

	return &serverConn{
		name:         name,
		client:       client,
		session:      session,
		capabilities: session.InitializeResult().Capabilities,
		cmd:          cmd,
	}, nil
}

func listAllTools(ctx context.Context, session *mcpsdk.ClientSession) ([]*mcpsdk.Tool, error) {
	var (
		cursor string
		tools  []*mcpsdk.Tool
	)
	for {
		params := &mcpsdk.ListToolsParams{Cursor: cursor}
		result, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if strings.TrimSpace(result.NextCursor) == "" {
			return tools, nil
		}
		cursor = result.NextCursor
	}
}

func listAllResources(ctx context.Context, session *mcpsdk.ClientSession) ([]*mcpsdk.Resource, error) {
	var (
		cursor    string
		resources []*mcpsdk.Resource
	)
	for {
		params := &mcpsdk.ListResourcesParams{Cursor: cursor}
		result, err := session.ListResources(ctx, params)
		if err != nil {
			return nil, err
		}
		resources = append(resources, result.Resources...)
		if strings.TrimSpace(result.NextCursor) == "" {
			return resources, nil
		}
		cursor = result.NextCursor
	}
}

func normalizeInputSchema(schema any) json.RawMessage {
	const defaultSchema = `{"type":"object","properties":{},"additionalProperties":true}`
	if schema == nil {
		return json.RawMessage(defaultSchema)
	}

	data, err := json.Marshal(schema)
	if err != nil || !json.Valid(data) {
		return json.RawMessage(defaultSchema)
	}

	// Ensure top-level schema is a JSON object.
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return json.RawMessage(defaultSchema)
	}
	if typ, _ := obj["type"].(string); typ == "" || typ == "object" {
		obj["type"] = "object"
		if _, ok := obj["properties"]; !ok {
			obj["properties"] = map[string]any{}
			normalized, err := json.Marshal(obj)
			if err == nil && json.Valid(normalized) {
				return json.RawMessage(normalized)
			}
		}
	}

	return json.RawMessage(data)
}
