package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerToolRequest is the immutable request passed to the host policy
// boundary. Approval and audit systems should persist RequestSHA256 and bind
// any decision to that digest rather than mutable display text.
type ServerToolRequest struct {
	TenantID      string
	Tool          agentsdk.Tool
	Arguments     json.RawMessage
	RequestSHA256 string
}

// ServerToolPolicy is the only execution path for tools exposed in MCP server
// mode. Implementations must apply the same tool-access checks, approvals,
// guardrails, tracing, quotas, and audit hooks as native SDK execution. Server
// mode intentionally never calls Tool.Execute directly.
type ServerToolPolicy interface {
	ExecuteMCPTool(context.Context, ServerToolRequest) (agentsdk.ToolResult, error)
}

// ServerToolPolicyFunc adapts a function to ServerToolPolicy.
type ServerToolPolicyFunc func(context.Context, ServerToolRequest) (agentsdk.ToolResult, error)

func (f ServerToolPolicyFunc) ExecuteMCPTool(ctx context.Context, request ServerToolRequest) (agentsdk.ToolResult, error) {
	return f(ctx, request)
}

// TenantResolver obtains the authenticated tenant from the HTTP request. An
// empty tenant fails closed before MCP protocol handling.
type TenantResolver interface {
	ResolveTenant(*http.Request) (string, error)
}

// TenantResolverFunc adapts a function to TenantResolver.
type TenantResolverFunc func(*http.Request) (string, error)

func (f TenantResolverFunc) ResolveTenant(request *http.Request) (string, error) { return f(request) }

const (
	maxServerModeSessions          = 1024
	maxServerModeSessionsPerTenant = 128
	serverModeSessionTTL           = 30 * time.Minute
)

type serverSessionBinding struct {
	tenant   string
	lastSeen time.Time
}

// ServerMode exposes an explicitly selected set of SDK tools through MCP while
// keeping execution behind a host-supplied policy boundary.
type ServerMode struct {
	server     *mcpsdk.Server
	handler    http.Handler
	policy     ServerToolPolicy
	tenants    TenantResolver
	sessionsMu sync.Mutex
	sessions   map[string]serverSessionBinding
	pending    map[string]int
	closed     bool
}

// ServerResource exposes one selected resource through a tenant-aware policy
// callback. The callback, not server mode, owns authorization and audit.
type ServerResource struct {
	Definition *mcpsdk.Resource
	Read       func(context.Context, string, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error)
}

// ServerPrompt exposes one selected prompt through a tenant-aware policy
// callback.
type ServerPrompt struct {
	Definition *mcpsdk.Prompt
	Get        func(context.Context, string, *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error)
}

// ServerModeOption adds policy-gated protocol surfaces.
type ServerModeOption func(*ServerMode) error

// WithServerResources adds explicitly selected tenant-aware resources.
func WithServerResources(resources ...ServerResource) ServerModeOption {
	return func(mode *ServerMode) error {
		for _, resource := range resources {
			if resource.Definition == nil || strings.TrimSpace(resource.Definition.URI) == "" || resource.Read == nil {
				return fmt.Errorf("MCP server resources require a definition and policy callback")
			}
			parsed, err := url.Parse(resource.Definition.URI)
			if err != nil || parsed.Scheme == "" {
				return fmt.Errorf("MCP server resource URI must be valid and absolute")
			}
			entry := resource
			mode.server.AddResource(entry.Definition, func(ctx context.Context, request *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				tenant, _ := ctx.Value(serverTenantContextKey{}).(string)
				if tenant == "" {
					return nil, fmt.Errorf("tenant context is missing")
				}
				result, err := entry.Read(ctx, tenant, request)
				if err != nil {
					return nil, fmt.Errorf("resource access denied or failed")
				}
				return result, nil
			})
		}
		return nil
	}
}

// WithServerPrompts adds explicitly selected tenant-aware prompts.
func WithServerPrompts(prompts ...ServerPrompt) ServerModeOption {
	return func(mode *ServerMode) error {
		for _, prompt := range prompts {
			if prompt.Definition == nil || strings.TrimSpace(prompt.Definition.Name) == "" || prompt.Get == nil {
				return fmt.Errorf("MCP server prompts require a definition and policy callback")
			}
			entry := prompt
			mode.server.AddPrompt(entry.Definition, func(ctx context.Context, request *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				tenant, _ := ctx.Value(serverTenantContextKey{}).(string)
				if tenant == "" {
					return nil, fmt.Errorf("tenant context is missing")
				}
				result, err := entry.Get(ctx, tenant, request)
				if err != nil {
					return nil, fmt.Errorf("prompt access denied or failed")
				}
				return result, nil
			})
		}
		return nil
	}
}

// NewServerMode creates a Streamable HTTP MCP server for selected tools.
// A policy executor and tenant resolver are mandatory; this prevents a
// convenience configuration from bypassing SDK authorization or tenant
// isolation. Callers remain responsible for serving the handler over verified
// TLS and authenticating requests before tenant resolution.
func NewServerMode(implementation *mcpsdk.Implementation, selected []agentsdk.Tool, policy ServerToolPolicy, tenants TenantResolver, options ...ServerModeOption) (*ServerMode, error) {
	if policy == nil {
		return nil, fmt.Errorf("MCP server mode requires a tool policy executor")
	}
	if tenants == nil {
		return nil, fmt.Errorf("MCP server mode requires a tenant resolver")
	}
	if implementation == nil {
		implementation = &mcpsdk.Implementation{Name: clientName, Version: clientVersion}
	}
	mode := &ServerMode{
		server:   mcpsdk.NewServer(implementation, nil),
		policy:   policy,
		tenants:  tenants,
		sessions: make(map[string]serverSessionBinding),
		pending:  make(map[string]int),
	}

	tools := append([]agentsdk.Tool(nil), selected...)
	for _, tool := range tools {
		if tool == nil || strings.TrimSpace(tool.Name()) == "" {
			return nil, fmt.Errorf("MCP server mode tool names must be non-empty")
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if _, exists := seen[tool.Name()]; exists {
			return nil, fmt.Errorf("duplicate MCP server mode tool %q", tool.Name())
		}
		seen[tool.Name()] = struct{}{}
		mode.addTool(tool)
	}
	for _, option := range options {
		if option != nil {
			if err := option(mode); err != nil {
				return nil, err
			}
		}
	}

	protocol := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mode.server }, &mcpsdk.StreamableHTTPOptions{SessionTimeout: serverModeSessionTTL})
	mode.handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if mode.isClosed() {
			http.Error(w, "MCP server is closed", http.StatusServiceUnavailable)
			return
		}
		tenant, err := mode.tenants.ResolveTenant(request)
		tenant = strings.TrimSpace(tenant)
		if err != nil || tenant == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sessionID := strings.TrimSpace(request.Header.Get("Mcp-Session-Id"))
		if sessionID != "" && !mode.sessionBelongsTo(sessionID, tenant) {
			http.Error(w, "MCP session tenant mismatch", http.StatusForbidden)
			return
		}
		if sessionID == "" {
			if !mode.reserveSessionSlot(tenant) {
				http.Error(w, "MCP session capacity reached", http.StatusTooManyRequests)
				return
			}
			defer mode.releaseSessionSlot(tenant)
		}
		ctx := context.WithValue(request.Context(), serverTenantContextKey{}, tenant)
		protocol.ServeHTTP(w, request.WithContext(ctx))
		if assigned := strings.TrimSpace(w.Header().Get("Mcp-Session-Id")); assigned != "" {
			mode.bindSession(assigned, tenant)
		}
		if request.Method == http.MethodDelete && sessionID != "" {
			mode.removeSession(sessionID)
		}
	})
	return mode, nil
}

// Handler returns the Streamable HTTP handler. It is safe for concurrent use.
func (s *ServerMode) Handler() http.Handler { return s.handler }

// Close rejects new requests and releases tenant-binding state. Underlying MCP
// sessions are bounded by serverModeSessionTTL and are closed by the protocol
// handler when they become idle.
func (s *ServerMode) Close() error {
	if s == nil {
		return nil
	}
	s.sessionsMu.Lock()
	s.closed = true
	s.sessions = make(map[string]serverSessionBinding)
	s.pending = make(map[string]int)
	s.sessionsMu.Unlock()
	return nil
}

func (s *ServerMode) isClosed() bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	return s.closed
}

func (s *ServerMode) sessionBelongsTo(sessionID, tenant string) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	binding, ok := s.sessions[sessionID]
	if !ok || binding.tenant != tenant || time.Since(binding.lastSeen) > serverModeSessionTTL {
		if ok && time.Since(binding.lastSeen) > serverModeSessionTTL {
			delete(s.sessions, sessionID)
		}
		return false
	}
	binding.lastSeen = time.Now()
	s.sessions[sessionID] = binding
	return true
}

func (s *ServerMode) reserveSessionSlot(tenant string) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.closed {
		return false
	}
	now := time.Now()
	for id, binding := range s.sessions {
		if now.Sub(binding.lastSeen) > serverModeSessionTTL {
			delete(s.sessions, id)
		}
	}
	tenantSessions := 0
	for _, binding := range s.sessions {
		if binding.tenant == tenant {
			tenantSessions++
		}
	}
	totalPending := 0
	for _, count := range s.pending {
		totalPending += count
	}
	if len(s.sessions)+totalPending >= maxServerModeSessions || tenantSessions+s.pending[tenant] >= maxServerModeSessionsPerTenant {
		return false
	}
	s.pending[tenant]++
	return true
}

func (s *ServerMode) releaseSessionSlot(tenant string) {
	s.sessionsMu.Lock()
	if s.pending[tenant] <= 1 {
		delete(s.pending, tenant)
	} else {
		s.pending[tenant]--
	}
	s.sessionsMu.Unlock()
}

func (s *ServerMode) bindSession(sessionID, tenant string) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.closed {
		return
	}
	if existing, ok := s.sessions[sessionID]; !ok || existing.tenant == tenant {
		s.sessions[sessionID] = serverSessionBinding{tenant: tenant, lastSeen: time.Now()}
	}
}

func (s *ServerMode) removeSession(sessionID string) {
	s.sessionsMu.Lock()
	delete(s.sessions, sessionID)
	s.sessionsMu.Unlock()
}

type serverTenantContextKey struct{}

func (s *ServerMode) addTool(tool agentsdk.Tool) {
	definition := &mcpsdk.Tool{
		Name:        tool.Name(),
		Description: tool.Description(),
		InputSchema: json.RawMessage(tool.InputSchema()),
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: tool.IsReadOnly()},
	}
	mcpsdk.AddTool(s.server, definition, func(ctx context.Context, _ *mcpsdk.CallToolRequest, arguments map[string]any) (*mcpsdk.CallToolResult, any, error) {
		tenant, _ := ctx.Value(serverTenantContextKey{}).(string)
		if tenant == "" {
			return nil, nil, fmt.Errorf("tenant context is missing")
		}
		raw, err := json.Marshal(arguments)
		if err != nil {
			return nil, nil, fmt.Errorf("encode tool arguments")
		}
		digest := serverRequestDigest(tenant, tool.Name(), raw)
		result, err := s.policy.ExecuteMCPTool(ctx, ServerToolRequest{
			TenantID:      tenant,
			Tool:          tool,
			Arguments:     append(json.RawMessage(nil), raw...),
			RequestSHA256: digest,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("tool execution denied or failed")
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: result.Content}},
			IsError: result.IsError,
		}, nil, nil
	})
}

func serverRequestDigest(tenant, tool string, arguments json.RawMessage) string {
	h := sha256.New()
	for _, value := range [][]byte{[]byte(tenant), []byte(tool), arguments} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(value)
	}
	return hex.EncodeToString(h.Sum(nil))
}
