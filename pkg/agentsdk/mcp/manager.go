package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultConnectTimeout = 15 * time.Second
	clientName            = "gratefulagents"
	clientVersion         = "0.1.0"
	// reconnectCooldown is the minimum interval between reconnect attempts
	// for a single server. Within the cooldown, callers get the original
	// call error instead of a fresh reconnect attempt.
	reconnectCooldown = 10 * time.Second
	maxDiscoveryPages = 100
	maxDiscoveryItems = 10_000
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

// PromptDescriptor describes an MCP prompt or prompt template.
type PromptDescriptor struct {
	Name        string                   `json:"name"`
	Title       string                   `json:"title,omitempty"`
	Description string                   `json:"description,omitempty"`
	Arguments   []*mcpsdk.PromptArgument `json:"arguments,omitempty"`
	Server      string                   `json:"server"`
}

type resourceCacheEntry struct {
	expires time.Time
	items   []ResourceDescriptor
}

type promptCacheEntry struct {
	expires time.Time
	items   []PromptDescriptor
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
	// cfg is retained for diagnostics and compatibility. reconnect constructs a
	// fresh connection without coupling the manager lifecycle to a transport.
	cfg       ServerConfig
	reconnect func(context.Context) (*serverConn, error)
	// remote is true for network transports. Remote descriptors are always
	// read-only in the experimental client.
	remote      bool
	cleanup     func()
	reflections *credentialReflections
	// stderr retains the tail of the child's stderr for diagnostics.
	stderr *stderrTail
}

// reconnectState serializes reconnect attempts for one server and rate-limits
// them to at most one per reconnectCooldown.
type reconnectState struct {
	mu          sync.Mutex
	lastAttempt time.Time
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
	// workDir and opts are retained so crashed stdio servers can be
	// reconnected mid-session.
	workDir string
	opts    managerOptions
	// reconnectMu guards reconnects; each reconnectState serializes
	// reconnect attempts for one server.
	reconnectMu sync.Mutex
	reconnects  map[string]*reconnectState
	// Discovery caches are tenant-local because a Manager owns exactly one set
	// of tenant-bound remote sessions.
	resourceCache map[string]resourceCacheEntry
	promptCache   map[string]promptCacheEntry
	closed        bool
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
	permissionMode       policy.PermissionMode
	executor             sandbox.Executor
	commandSandboxConfig *sandbox.Config
	networkAccessServers map[string]struct{}
	remote               remoteManagerOptions
	discoveryCacheTTL    time.Duration
}

// ManagerOption configures MCP transport, lifecycle, and policy behavior.
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

// WithNetworkAccessForServers permits network access for the named MCP servers.
// Hosts must only pass names approved by a trusted policy; repository MCP
// configuration cannot enable network access on its own.
func WithNetworkAccessForServers(names ...string) ManagerOption {
	return func(opts *managerOptions) {
		if opts.networkAccessServers == nil {
			opts.networkAccessServers = make(map[string]struct{})
		}
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name != "" {
				opts.networkAccessServers[name] = struct{}{}
			}
		}
	}
}

// WithCommandSandboxConfig sets the sandbox configuration used for MCP stdio
// subprocesses. The manager always replaces WorkspaceRoot with its trusted
// workDir; callers cannot redirect the writable workspace through this option.
func WithCommandSandboxConfig(config sandbox.Config) ManagerOption {
	return func(opts *managerOptions) {
		opts.commandSandboxConfig = &config
	}
}

// WithDiscoveryCacheTTL sets the tool/resource/prompt discovery cache lifetime.
// A non-positive duration disables resource and prompt caching. Tool discovery
// remains pinned for the manager lifetime to prevent mid-run capability drift.
func WithDiscoveryCacheTTL(ttl time.Duration) ManagerOption {
	return func(opts *managerOptions) { opts.discoveryCacheTTL = ttl }
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
	options := resolveManagerOptions(workDir, opts...)
	m := &Manager{
		servers:             make(map[string]*serverConn),
		toolByQualifiedName: make(map[string]ToolDescriptor),
		workDir:             workDir,
		opts:                options,
		reconnects:          make(map[string]*reconnectState),
		resourceCache:       make(map[string]resourceCacheEntry),
		promptCache:         make(map[string]promptCacheEntry),
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

		conn, err := connectConfiguredServer(ctx, workDir, serverName, srvCfg, options)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.servers[serverName] = conn
		if conn.remote {
			if err := auditRemote(ctx, options, serverName, "tools/list", "attempted"); err != nil {
				_ = closeSessionBounded(conn, 2*time.Second)
				delete(m.servers, serverName)
				errs = append(errs, fmt.Errorf("MCP server %q: remote audit unavailable", serverName))
				continue
			}
		}

		tools, err := listAllTools(ctx, conn.session)
		if err != nil {
			if conn.remote {
				_ = closeSessionBounded(conn, 2*time.Second)
				delete(m.servers, serverName)
				errs = append(errs, fmt.Errorf("MCP server %q: remote list tools failed", serverName))
			} else {
				errs = append(errs, errWithStderr(
					fmt.Errorf("MCP server %q: list tools: %w", serverName, err),
					conn.stderr.tailAfterGrace(250*time.Millisecond)))
			}
			continue
		}
		if conn.remote {
			if err := auditRemote(ctx, options, serverName, "tools/list", "completed"); err != nil {
				_ = closeSessionBounded(conn, 2*time.Second)
				delete(m.servers, serverName)
				errs = append(errs, fmt.Errorf("MCP server %q: remote audit unavailable after discovery", serverName))
				continue
			}
		}
		if conn.remote && conn.reflections.Contains(tools) {
			_ = closeSessionBounded(conn, 2*time.Second)
			delete(m.servers, serverName)
			errs = append(errs, fmt.Errorf("MCP server %q: credential reflection blocked during tool discovery", serverName))
			continue
		}

		for _, tool := range tools {
			if tool == nil {
				continue
			}
			if !serverToolAllowed(srvCfg, tool.Name) {
				continue
			}
			readOnly := trustedMCPReadOnly(srvCfg, tool)
			// Remote execution is experimental and read-only. A remote server's
			// mutation-capable or untrusted descriptor is never registered, so it
			// cannot be reached by guessing a qualified tool name.
			if conn.remote && (!readOnly || !remoteToolAllowedByHost(options, serverName, tool.Name)) {
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
				ReadOnly:           readOnly,
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

func resolveManagerOptions(workDir string, opts ...ManagerOption) managerOptions {
	options := managerOptions{
		permissionMode:    policy.PermissionModeWorkspaceWrite,
		discoveryCacheTTL: 30 * time.Second,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.executor == nil {
		config := sandbox.ConfigFromEnv()
		if options.commandSandboxConfig != nil {
			config = *options.commandSandboxConfig
		}
		// workDir is supplied by the host and is the manager's trust boundary.
		// Restricted sandboxes must receive it explicitly rather than inferring a
		// writable root from an MCP-controlled command working directory.
		config.WorkspaceRoot = workDir
		options.executor = sandbox.DefaultWithConfig(config)
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
	m.closed = true
	servers := make([]*serverConn, 0, len(m.servers))
	for _, conn := range m.servers {
		servers = append(servers, conn)
	}
	m.servers = map[string]*serverConn{}
	m.toolDescriptors = nil
	m.toolByQualifiedName = map[string]ToolDescriptor{}
	m.resourceCache = map[string]resourceCacheEntry{}
	m.promptCache = map[string]promptCacheEntry{}
	m.mu.Unlock()

	var errs []error
	for _, conn := range servers {
		if conn == nil {
			continue
		}
		if err := closeSessionBounded(conn, 2*time.Second); err != nil {
			errs = append(errs, fmt.Errorf("closing MCP server %q: %w", conn.name, err))
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

func closeSessionBounded(conn *serverConn, timeout time.Duration) error {
	if conn == nil {
		return nil
	}
	if conn.session == nil {
		if conn.cleanup != nil {
			conn.cleanup()
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- conn.session.Close() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if conn.cleanup != nil {
			conn.cleanup()
		}
		return err
	case <-timer.C:
		if conn.cleanup != nil {
			conn.cleanup()
		}
	}
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("session close timed out")
	}
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
	if conn.remote && (!desc.ReadOnly || !remoteToolAllowedByHost(m.opts, desc.ServerName, desc.ToolName)) {
		return nil, fmt.Errorf("MCP remote tool %q is not allowed by host read-only policy", qualifiedName)
	}

	if conn.remote {
		if err := auditRemote(ctx, m.opts, desc.ServerName, "tools/call:"+desc.ToolName, "attempted"); err != nil {
			return nil, fmt.Errorf("MCP remote audit unavailable")
		}
	}
	params := &mcpsdk.CallToolParams{
		Name:      desc.ToolName,
		Arguments: args,
	}
	attempt := &remoteAttemptState{}
	callCtx := ctx
	if conn.remote {
		callCtx = context.WithValue(ctx, remoteAttemptContextKey{}, attempt)
	}
	result, err := conn.session.CallTool(callCtx, params)
	if err != nil && conn.remote {
		if errors.Is(err, errRemoteNotSent) {
			return nil, fmt.Errorf("MCP remote request was not sent")
		}
		if errors.Is(err, errRemoteDefinitive) {
			return nil, fmt.Errorf("MCP remote server returned a definitive HTTP error")
		}
		if !attempt.ambiguousHTTP.Load() && !errors.Is(err, errRemoteAmbiguous) {
			var rpcErr *jsonrpc.Error
			if errors.As(err, &rpcErr) {
				return nil, fmt.Errorf("MCP remote server returned a definitive error")
			}
		}
		// Never replay a remote tools/call. A disconnect or timeout may occur
		// after the server applied the operation. Reconnect only prepares the
		// next independent request; this one remains reconciliation-required.
		if isSessionClosedErr(err) && ctx.Err() == nil {
			_, _ = m.reconnectServer(ctx, desc.ServerName, conn)
		}
		auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_ = auditRemote(auditCtx, m.opts, desc.ServerName, "tools/call:"+desc.ToolName, "outcome-unknown")
		cancelAudit()
		return nil, &OutcomeUnknownError{Server: desc.ServerName, Tool: desc.ToolName}
	}
	if err != nil && isSessionClosedErr(err) {
		if fresh, rerr := m.reconnectServer(ctx, desc.ServerName, conn); rerr == nil {
			result, err = fresh.session.CallTool(ctx, params)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("MCP %s/%s: %w", desc.ServerName, desc.ToolName, err)
	}
	if conn.remote && conn.reflections.Contains(result) {
		_ = auditRemote(ctx, m.opts, desc.ServerName, "tools/call:"+desc.ToolName, "credential-reflection-blocked")
		return nil, fmt.Errorf("MCP remote response contained credential material and was blocked")
	}
	if conn.remote {
		if err := auditRemote(ctx, m.opts, desc.ServerName, "tools/call:"+desc.ToolName, "completed"); err != nil {
			return nil, fmt.Errorf("MCP remote audit unavailable after call")
		}
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
		resources, err := m.cachedResources(ctx, conn)
		if err != nil {
			errs = append(errs, fmt.Errorf("MCP server %q: list resources: %w", conn.name, err))
			continue
		}
		out = append(out, resources...)
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

func (m *Manager) cachedResources(ctx context.Context, conn *serverConn) ([]ResourceDescriptor, error) {
	now := time.Now()
	m.mu.RLock()
	cached, ok := m.resourceCache[conn.name]
	ttl := m.opts.discoveryCacheTTL
	m.mu.RUnlock()
	if ttl > 0 && ok && now.Before(cached.expires) {
		return append([]ResourceDescriptor(nil), cached.items...), nil
	}
	if conn.remote {
		if err := auditRemote(ctx, m.opts, conn.name, "resources/list", "attempted"); err != nil {
			return nil, fmt.Errorf("remote audit unavailable")
		}
	}
	resources, err := listAllResources(ctx, conn.session)
	if err != nil {
		if conn.remote {
			return nil, fmt.Errorf("remote resource discovery failed")
		}
		return nil, err
	}
	if conn.remote && conn.reflections.Contains(resources) {
		_ = auditRemote(ctx, m.opts, conn.name, "resources/list", "credential-reflection-blocked")
		return nil, fmt.Errorf("remote resource discovery contained credential material and was blocked")
	}
	if conn.remote {
		if err := auditRemote(ctx, m.opts, conn.name, "resources/list", "completed"); err != nil {
			return nil, fmt.Errorf("remote audit unavailable after resource discovery")
		}
	}
	items := make([]ResourceDescriptor, 0, len(resources))
	for _, resource := range resources {
		if resource != nil {
			items = append(items, ResourceDescriptor{URI: resource.URI, Name: resource.Name, MIMEType: resource.MIMEType, Description: resource.Description, Server: conn.name})
		}
	}
	if ttl > 0 {
		m.mu.Lock()
		m.resourceCache[conn.name] = resourceCacheEntry{expires: now.Add(ttl), items: append([]ResourceDescriptor(nil), items...)}
		m.mu.Unlock()
	}
	return items, nil
}

// ListPrompts returns cached prompt discovery for all servers or one server.
func (m *Manager) ListPrompts(ctx context.Context, serverName string) ([]PromptDescriptor, error) {
	if m == nil {
		return nil, fmt.Errorf("MCP manager is not initialized")
	}
	targets, err := m.capabilityServers(serverName, func(c *serverConn) bool {
		return c.capabilities != nil && c.capabilities.Prompts != nil
	}, "prompts")
	if err != nil {
		return nil, err
	}
	var out []PromptDescriptor
	var errs []error
	for _, conn := range targets {
		items, err := m.cachedPrompts(ctx, conn)
		if err != nil {
			errs = append(errs, fmt.Errorf("MCP server %q: list prompts: %w", conn.name, err))
			continue
		}
		out = append(out, items...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server == out[j].Server {
			return out[i].Name < out[j].Name
		}
		return out[i].Server < out[j].Server
	})
	if len(out) > 0 {
		return out, nil
	}
	return nil, errors.Join(errs...)
}

func (m *Manager) cachedPrompts(ctx context.Context, conn *serverConn) ([]PromptDescriptor, error) {
	now := time.Now()
	m.mu.RLock()
	cached, ok := m.promptCache[conn.name]
	ttl := m.opts.discoveryCacheTTL
	m.mu.RUnlock()
	if ttl > 0 && ok && now.Before(cached.expires) {
		return clonePromptDescriptors(cached.items), nil
	}
	if conn.remote {
		if err := auditRemote(ctx, m.opts, conn.name, "prompts/list", "attempted"); err != nil {
			return nil, fmt.Errorf("remote audit unavailable")
		}
	}
	prompts, err := listAllPrompts(ctx, conn.session)
	if err != nil {
		if conn.remote {
			return nil, fmt.Errorf("remote prompt discovery failed")
		}
		return nil, err
	}
	if conn.remote && conn.reflections.Contains(prompts) {
		_ = auditRemote(ctx, m.opts, conn.name, "prompts/list", "credential-reflection-blocked")
		return nil, fmt.Errorf("remote prompt discovery contained credential material and was blocked")
	}
	if conn.remote {
		if err := auditRemote(ctx, m.opts, conn.name, "prompts/list", "completed"); err != nil {
			return nil, fmt.Errorf("remote audit unavailable after prompt discovery")
		}
	}
	items := make([]PromptDescriptor, 0, len(prompts))
	for _, prompt := range prompts {
		if prompt != nil {
			items = append(items, PromptDescriptor{Name: prompt.Name, Title: prompt.Title, Description: prompt.Description, Arguments: prompt.Arguments, Server: conn.name})
		}
	}
	if ttl > 0 {
		m.mu.Lock()
		m.promptCache[conn.name] = promptCacheEntry{expires: now.Add(ttl), items: clonePromptDescriptors(items)}
		m.mu.Unlock()
	}
	return items, nil
}

func clonePromptDescriptors(items []PromptDescriptor) []PromptDescriptor {
	out := make([]PromptDescriptor, len(items))
	for i, item := range items {
		out[i] = item
		out[i].Arguments = make([]*mcpsdk.PromptArgument, len(item.Arguments))
		for j, argument := range item.Arguments {
			if argument != nil {
				copy := *argument
				out[i].Arguments[j] = &copy
			}
		}
	}
	return out
}

// GetPrompt renders a named prompt on a specific server.
func (m *Manager) GetPrompt(ctx context.Context, serverName, name string, arguments map[string]string) (*mcpsdk.GetPromptResult, error) {
	if m == nil {
		return nil, fmt.Errorf("MCP manager is not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("prompt name is required")
	}
	conn, err := m.getServer(serverName)
	if err != nil {
		return nil, err
	}
	if conn.capabilities == nil || conn.capabilities.Prompts == nil {
		return nil, fmt.Errorf("MCP server %q does not support prompts", serverName)
	}
	operation := "prompts/get:" + name
	if conn.remote {
		if err := auditRemote(ctx, m.opts, serverName, operation, "attempted"); err != nil {
			return nil, fmt.Errorf("MCP remote audit unavailable")
		}
	}
	result, err := conn.session.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: name, Arguments: arguments})
	if err != nil && isSessionClosedErr(err) {
		if fresh, reconnectErr := m.reconnectServer(ctx, serverName, conn); reconnectErr == nil {
			conn = fresh
			if conn.remote {
				if auditErr := auditRemote(ctx, m.opts, serverName, operation, "retry-attempted"); auditErr != nil {
					return nil, fmt.Errorf("MCP remote audit unavailable before retry")
				}
			}
			result, err = fresh.session.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: name, Arguments: arguments})
		}
	}
	if err != nil {
		if conn.remote {
			return nil, fmt.Errorf("MCP server %q get prompt %q failed", serverName, name)
		}
		return nil, fmt.Errorf("MCP server %q get prompt %q: %w", serverName, name, err)
	}
	if conn.remote && conn.reflections.Contains(result) {
		_ = auditRemote(ctx, m.opts, serverName, operation, "credential-reflection-blocked")
		return nil, fmt.Errorf("MCP remote prompt contained credential material and was blocked")
	}
	if conn.remote {
		if err := auditRemote(ctx, m.opts, serverName, operation, "completed"); err != nil {
			return nil, fmt.Errorf("MCP remote audit unavailable after prompt request")
		}
	}
	return result, nil
}

// InvalidateDiscovery clears resource and prompt caches. An empty server name
// invalidates every server. Hosts can call this from MCP list-changed hooks.
func (m *Manager) InvalidateDiscovery(serverName string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if serverName == "" {
		m.resourceCache = make(map[string]resourceCacheEntry)
		m.promptCache = make(map[string]promptCacheEntry)
		return
	}
	delete(m.resourceCache, serverName)
	delete(m.promptCache, serverName)
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
	operation := "resources/read"
	if conn.remote {
		if err := auditRemote(ctx, m.opts, serverName, operation, "attempted"); err != nil {
			return nil, fmt.Errorf("MCP remote audit unavailable")
		}
	}

	result, err := conn.session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil && isSessionClosedErr(err) {
		if fresh, rerr := m.reconnectServer(ctx, serverName, conn); rerr == nil {
			conn = fresh
			if conn.remote {
				if auditErr := auditRemote(ctx, m.opts, serverName, operation, "retry-attempted"); auditErr != nil {
					return nil, fmt.Errorf("MCP remote audit unavailable before retry")
				}
			}
			result, err = fresh.session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
		}
	}
	if err != nil {
		if conn.remote {
			return nil, fmt.Errorf("MCP server %q resource read failed", serverName)
		}
		return nil, fmt.Errorf("MCP server %q read %q: %w", serverName, uri, err)
	}
	if conn.remote && conn.reflections.Contains(result) {
		_ = auditRemote(ctx, m.opts, serverName, operation, "credential-reflection-blocked")
		return nil, fmt.Errorf("MCP remote resource contained credential material and was blocked")
	}
	if conn.remote {
		if err := auditRemote(ctx, m.opts, serverName, operation, "completed"); err != nil {
			return nil, fmt.Errorf("MCP remote audit unavailable after resource read")
		}
	}
	return result, nil
}

// isSessionClosedErr reports whether err indicates the MCP session or its
// transport is gone (subprocess died, pipe closed), i.e. a reconnect may help.
func isSessionClosedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mcpsdk.ErrConnectionClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "file already closed") ||
		strings.Contains(msg, "process already finished")
}

// shouldAttemptReconnect reports whether a reconnect attempt is allowed given
// the previous attempt time. A zero lastAttempt means no attempt has been made.
func shouldAttemptReconnect(lastAttempt, now time.Time, cooldown time.Duration) bool {
	if lastAttempt.IsZero() {
		return true
	}
	return now.Sub(lastAttempt) >= cooldown
}

func (m *Manager) reconnectStateFor(name string) *reconnectState {
	m.reconnectMu.Lock()
	defer m.reconnectMu.Unlock()
	if m.reconnects == nil {
		m.reconnects = make(map[string]*reconnectState)
	}
	st, ok := m.reconnects[name]
	if !ok {
		st = &reconnectState{}
		m.reconnects[name] = st
	}
	return st
}

// reconnectServer attempts a single transport-neutral reconnect and returns
// the replacement connection. failed is the connection the caller observed the
// failure on; if another goroutine already replaced it, the current connection
// is returned. Attempts are rate-limited per server.
//
// Note: tool descriptors are NOT refreshed on reconnect — the tool list pinned
// at construction time keeps serving; a server that changes its tools across
// restarts is not supported mid-session.
func (m *Manager) reconnectServer(ctx context.Context, name string, failed *serverConn) (*serverConn, error) {
	st := m.reconnectStateFor(name)
	st.mu.Lock()
	defer st.mu.Unlock()

	m.mu.RLock()
	current := m.servers[name]
	m.mu.RUnlock()
	if current == nil {
		return nil, fmt.Errorf("MCP server %q is no longer registered", name)
	}
	if current != failed {
		// A concurrent caller already reconnected; reuse its connection.
		return current, nil
	}

	if !shouldAttemptReconnect(st.lastAttempt, time.Now(), reconnectCooldown) {
		return nil, fmt.Errorf("MCP server %q: reconnect attempted too recently", name)
	}
	st.lastAttempt = time.Now()

	if current.reconnect == nil {
		return nil, fmt.Errorf("MCP server %q does not support reconnect", name)
	}
	fresh, err := current.reconnect(ctx)
	if err != nil {
		return nil, fmt.Errorf("MCP server %q reconnect failed", name)
	}

	m.mu.Lock()
	if m.closed || m.servers[name] != current {
		m.mu.Unlock()
		_ = closeSessionBounded(fresh, 2*time.Second)
		_ = terminateProcess(fresh.cmd, 2*time.Second)
		return nil, fmt.Errorf("MCP server %q manager closed or connection changed during reconnect", name)
	}
	old := m.servers[name]
	m.servers[name] = fresh
	delete(m.resourceCache, name)
	delete(m.promptCache, name)
	m.mu.Unlock()

	if old != nil {
		_ = closeSessionBounded(old, 2*time.Second)
		_ = terminateProcess(old.cmd, 2*time.Second)
	}
	return fresh, nil
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

func (m *Manager) capabilityServers(serverName string, supported func(*serverConn) bool, capability string) ([]*serverConn, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if serverName != "" {
		conn, ok := m.servers[serverName]
		if !ok {
			return nil, fmt.Errorf("MCP server %q not found", serverName)
		}
		if !supported(conn) {
			return nil, fmt.Errorf("MCP server %q does not support %s", serverName, capability)
		}
		return []*serverConn{conn}, nil
	}
	var servers []*serverConn
	for _, conn := range m.servers {
		if conn != nil && supported(conn) {
			servers = append(servers, conn)
		}
	}
	return servers, nil
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

func connectConfiguredServer(ctx context.Context, workDir, name string, cfg ServerConfig, opts managerOptions) (*serverConn, error) {
	transportType := strings.ToLower(strings.TrimSpace(cfg.Type))
	switch transportType {
	case "", TransportStdio:
		if strings.TrimSpace(cfg.URL) != "" {
			return nil, fmt.Errorf("MCP server %q: stdio transport cannot set url", name)
		}
		return connectStdioServer(ctx, workDir, name, cfg, opts)
	case TransportStreamableHTTP, TransportLegacySSE:
		if strings.TrimSpace(cfg.Command) != "" || len(cfg.Args) != 0 || len(cfg.Env) != 0 || len(cfg.AllowEnv) != 0 {
			return nil, fmt.Errorf("MCP server %q: remote transport cannot set command, args, env, or allowEnv", name)
		}
		return connectRemoteServer(ctx, name, cfg, opts)
	default:
		return nil, fmt.Errorf("MCP server %q uses unsupported type %q", name, cfg.Type)
	}
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

	// Build the child on a context that is NOT the connect timeout: executors
	// use exec.CommandContext, so a child bound to connectCtx is SIGKILLed the
	// moment this function returns and the deferred cancel() runs — every
	// server then died silently right after a successful handshake, and the
	// first tools/list read EOF from the dead pipe (with nothing on stderr).
	// The connect timeout below applies to the handshake only; process
	// lifetime is owned by the manager (Close/terminateProcess) and the
	// sandbox's --die-with-parent.
	_, allowNetwork := opts.networkAccessServers[name]
	cmd, err := opts.executor.Build(context.WithoutCancel(ctx), sandbox.Request{
		Argv:           append([]string{command}, args...),
		WorkDir:        workDir,
		PermissionMode: opts.permissionMode,
		Env:            filteredEnv(name, cfg),
		AllowNetwork:   allowNetwork,
	})
	if err != nil {
		return nil, fmt.Errorf("MCP server %q sandbox: %w", name, err)
	}
	configureProcessGroup(cmd)
	// Capture the child's stderr so failures report the real reason (panic,
	// traceback, missing binary chatter) instead of a bare EOF. The go-sdk
	// CommandTransport does not touch cmd.Stderr.
	stderr := newStderrTail(stderrTailCap)
	cmd.Stderr = stderr

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    clientName,
		Version: clientVersion,
	}, nil)

	session, err := client.Connect(connectCtx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, errWithStderr(fmt.Errorf("MCP server %q connect: %w", name, err),
			stderr.tailAfterGrace(250*time.Millisecond))
	}

	conn := &serverConn{
		name:         name,
		client:       client,
		session:      session,
		capabilities: session.InitializeResult().Capabilities,
		cmd:          cmd,
		cfg:          cfg,
		stderr:       stderr,
	}
	conn.reconnect = func(reconnectCtx context.Context) (*serverConn, error) {
		return connectStdioServer(reconnectCtx, workDir, name, cfg, opts)
	}
	return conn, nil
}

func listAllTools(ctx context.Context, session *mcpsdk.ClientSession) ([]*mcpsdk.Tool, error) {
	var cursor string
	var tools []*mcpsdk.Tool
	seen := make(map[string]struct{})
	for page := 0; page < maxDiscoveryPages; page++ {
		result, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if len(tools)+len(result.Tools) > maxDiscoveryItems {
			return nil, fmt.Errorf("MCP tool discovery exceeded item limit")
		}
		tools = append(tools, result.Tools...)
		next := strings.TrimSpace(result.NextCursor)
		if next == "" {
			return tools, nil
		}
		if _, duplicate := seen[next]; duplicate {
			return nil, fmt.Errorf("MCP tool discovery repeated cursor")
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return nil, fmt.Errorf("MCP tool discovery exceeded page limit")
}

func listAllResources(ctx context.Context, session *mcpsdk.ClientSession) ([]*mcpsdk.Resource, error) {
	var cursor string
	var resources []*mcpsdk.Resource
	seen := make(map[string]struct{})
	for page := 0; page < maxDiscoveryPages; page++ {
		result, err := session.ListResources(ctx, &mcpsdk.ListResourcesParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if len(resources)+len(result.Resources) > maxDiscoveryItems {
			return nil, fmt.Errorf("MCP resource discovery exceeded item limit")
		}
		resources = append(resources, result.Resources...)
		next := strings.TrimSpace(result.NextCursor)
		if next == "" {
			return resources, nil
		}
		if _, duplicate := seen[next]; duplicate {
			return nil, fmt.Errorf("MCP resource discovery repeated cursor")
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return nil, fmt.Errorf("MCP resource discovery exceeded page limit")
}

func listAllPrompts(ctx context.Context, session *mcpsdk.ClientSession) ([]*mcpsdk.Prompt, error) {
	var cursor string
	var prompts []*mcpsdk.Prompt
	seen := make(map[string]struct{})
	for page := 0; page < maxDiscoveryPages; page++ {
		result, err := session.ListPrompts(ctx, &mcpsdk.ListPromptsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if len(prompts)+len(result.Prompts) > maxDiscoveryItems {
			return nil, fmt.Errorf("MCP prompt discovery exceeded item limit")
		}
		prompts = append(prompts, result.Prompts...)
		next := strings.TrimSpace(result.NextCursor)
		if next == "" {
			return prompts, nil
		}
		if _, duplicate := seen[next]; duplicate {
			return nil, fmt.Errorf("MCP prompt discovery repeated cursor")
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return nil, fmt.Errorf("MCP prompt discovery exceeded page limit")
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
	mutated := false
	if typ, _ := obj["type"].(string); typ == "" || typ == "object" {
		if typ != "object" {
			obj["type"] = "object"
			mutated = true
		}
		if _, ok := obj["properties"]; !ok {
			obj["properties"] = map[string]any{}
			mutated = true
		}
	}
	if mutated {
		normalized, err := json.Marshal(obj)
		if err != nil || !json.Valid(normalized) {
			return json.RawMessage(defaultSchema)
		}
		return json.RawMessage(normalized)
	}

	return json.RawMessage(data)
}
