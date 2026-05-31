package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// BreakGlassRequestResult is the host-neutral result returned by a
// BreakGlassSink after it persists or rejects a request.
type BreakGlassRequestResult struct {
	Content     string
	IsError     bool
	ShouldPause bool
}

// BreakGlassSink persists a break-glass request in the host platform and, when
// appropriate, asks the host UI to collect approval.
type BreakGlassSink interface {
	RequestMCPBreakGlass(ctx context.Context, request BreakGlassRequest) (BreakGlassRequestResult, error)
}

// RequestBreakGlassTool is the SDK-owned shell for MCP break-glass requests.
// Hosts provide only the persistence/approval sink.
type RequestBreakGlassTool struct {
	Sink BreakGlassSink
}

func (t *RequestBreakGlassTool) Name() string { return "RequestMCPBreakGlass" }

func (t *RequestBreakGlassTool) Description() string {
	return "Request temporary MCP server or tool access when policy blocks it. This pauses the run and asks a human to approve or deny the request."
}

func (t *RequestBreakGlassTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"server":{"type":"string","description":"Connected MCP server name that needs temporary access"},
			"tool":{"type":"string","description":"Optional tool name on that server. Leave empty to request the entire server."},
			"reason":{"type":"string","description":"Short audit reason for why the access is needed"}
		},
		"required":["server"]
	}`)
}

func (t *RequestBreakGlassTool) IsReadOnly() bool                      { return false }
func (t *RequestBreakGlassTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }
func (t *RequestBreakGlassTool) NeedsApproval() bool                   { return false }
func (t *RequestBreakGlassTool) TimeoutSeconds() int                   { return 0 }

func (t *RequestBreakGlassTool) Execute(ctx context.Context, input json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Server string `json:"server"`
		Tool   string `json:"tool"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}

	request := BreakGlassRequest{
		Server: strings.TrimSpace(in.Server),
		Tool:   strings.TrimSpace(in.Tool),
		Reason: strings.TrimSpace(in.Reason),
	}
	if request.Server == "" {
		return agentsdk.ToolResult{Content: "server is required", IsError: true}, nil
	}
	if t.Sink == nil {
		return agentsdk.ToolResult{Content: "MCP break-glass sink is not configured", IsError: true}, nil
	}

	result, err := t.Sink.RequestMCPBreakGlass(ctx, request)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agentsdk.ToolResult{
		Content:     result.Content,
		IsError:     result.IsError,
		ShouldPause: result.ShouldPause,
	}, nil
}

// BreakGlassCatalog contains the connected MCP servers/tools that can be
// requested through a break-glass workflow.
type BreakGlassCatalog struct {
	servers map[string]struct{}
	tools   map[string]map[string]struct{}
}

func NewBreakGlassCatalog(descriptors []ToolDescriptor, servers []string) BreakGlassCatalog {
	catalog := BreakGlassCatalog{
		servers: make(map[string]struct{}),
		tools:   make(map[string]map[string]struct{}),
	}
	for _, server := range servers {
		if trimmed := strings.TrimSpace(server); trimmed != "" {
			catalog.servers[trimmed] = struct{}{}
		}
	}
	for _, desc := range descriptors {
		server := strings.TrimSpace(desc.ServerName)
		tool := strings.TrimSpace(desc.ToolName)
		if server == "" {
			continue
		}
		catalog.servers[server] = struct{}{}
		if tool == "" {
			continue
		}
		if _, ok := catalog.tools[server]; !ok {
			catalog.tools[server] = make(map[string]struct{})
		}
		catalog.tools[server][tool] = struct{}{}
	}
	return catalog
}

func (c BreakGlassCatalog) Validate(server, tool string) error {
	server = strings.TrimSpace(server)
	tool = strings.TrimSpace(tool)
	if server == "" {
		return fmt.Errorf("server is required")
	}
	if _, ok := c.servers[server]; !ok {
		return fmt.Errorf("unknown MCP server %q", server)
	}
	if tool == "" {
		return nil
	}
	tools, ok := c.tools[server]
	if !ok {
		return fmt.Errorf("server %q does not expose tools that can be requested individually", server)
	}
	if _, ok := tools[tool]; !ok {
		return fmt.Errorf("unknown MCP tool %q on server %q", tool, server)
	}
	return nil
}
