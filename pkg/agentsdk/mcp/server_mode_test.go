package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerModeUsesPolicyBoundaryAndImmutableRequest(t *testing.T) {
	t.Parallel()
	var directCalls atomic.Int32
	tool := &agentsdk.FunctionTool{
		ToolName:        "lookup",
		ToolDescription: "look up a value",
		Schema:          json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
		ReadOnly:        true,
		Fn: func(context.Context, json.RawMessage) (string, error) {
			directCalls.Add(1)
			return "bypassed", nil
		},
	}
	var policyRequest ServerToolRequest
	mode, err := NewServerMode(
		&mcpsdk.Implementation{Name: "sdk", Version: "1"},
		[]agentsdk.Tool{tool},
		ServerToolPolicyFunc(func(_ context.Context, request ServerToolRequest) (agentsdk.ToolResult, error) {
			policyRequest = request
			return agentsdk.ToolResult{Content: "policy result"}, nil
		}),
		TenantResolverFunc(func(request *http.Request) (string, error) {
			return request.Header.Get("X-Tenant"), nil
		}),
		WithServerResources(ServerResource{
			Definition: &mcpsdk.Resource{URI: "memory://note/1", Name: "note"},
			Read: func(_ context.Context, tenant string, _ *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: "memory://note/1", Text: tenant + " note"}}}, nil
			},
		}),
		WithServerPrompts(ServerPrompt{
			Definition: &mcpsdk.Prompt{Name: "summarize"},
			Get: func(_ context.Context, tenant string, _ *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				return &mcpsdk.GetPromptResult{Messages: []*mcpsdk.PromptMessage{{Role: mcpsdk.Role("user"), Content: &mcpsdk.TextContent{Text: tenant + " prompt"}}}}, nil
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewServerMode: %v", err)
	}
	httpServer := httptest.NewServer(mode.Handler())
	defer httpServer.Close()
	unauthorized, err := http.Post(httpServer.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("unauthorized request: %v", err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.StatusCode)
	}

	httpClient := &http.Client{Transport: headerTransport{
		base:   http.DefaultTransport,
		header: http.Header{"X-Tenant": []string{"tenant-a"}},
	}}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcpsdk.StreamableClientTransport{Endpoint: httpServer.URL, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	crossTenantRequest, err := http.NewRequest(http.MethodPost, httpServer.URL, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	crossTenantRequest.Header.Set("Content-Type", "application/json")
	crossTenantRequest.Header.Set("Accept", "application/json, text/event-stream")
	crossTenantRequest.Header.Set("X-Tenant", "tenant-b")
	crossTenantRequest.Header.Set("Mcp-Session-Id", session.ID())
	crossTenantResponse, err := http.DefaultClient.Do(crossTenantRequest)
	if err != nil {
		t.Fatalf("cross-tenant request: %v", err)
	}
	crossTenantResponse.Body.Close()
	if crossTenantResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant status = %d, want 403", crossTenantResponse.StatusCode)
	}
	result, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "lookup", Arguments: map[string]any{"id": "42"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if directCalls.Load() != 0 {
		t.Fatal("server mode called Tool.Execute directly and bypassed policy")
	}
	if policyRequest.TenantID != "tenant-a" || policyRequest.Tool != tool {
		t.Fatalf("policy request = %#v", policyRequest)
	}
	if len(policyRequest.RequestSHA256) != 64 || policyRequest.RequestSHA256 != serverRequestDigest("tenant-a", "lookup", policyRequest.Arguments) {
		t.Fatalf("request digest = %q, want immutable request digest", policyRequest.RequestSHA256)
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].(*mcpsdk.TextContent).Text, "policy result") {
		t.Fatalf("result = %#v", result)
	}
	resources, err := session.ListResources(t.Context(), nil)
	if err != nil || len(resources.Resources) != 1 {
		t.Fatalf("ListResources = %#v, %v", resources, err)
	}
	resource, err := session.ReadResource(t.Context(), &mcpsdk.ReadResourceParams{URI: "memory://note/1"})
	if err != nil || len(resource.Contents) != 1 || resource.Contents[0].Text != "tenant-a note" {
		t.Fatalf("ReadResource = %#v, %v", resource, err)
	}
	prompts, err := session.ListPrompts(t.Context(), nil)
	if err != nil || len(prompts.Prompts) != 1 {
		t.Fatalf("ListPrompts = %#v, %v", prompts, err)
	}
	prompt, err := session.GetPrompt(t.Context(), &mcpsdk.GetPromptParams{Name: "summarize"})
	if err != nil || len(prompt.Messages) != 1 || prompt.Messages[0].Content.(*mcpsdk.TextContent).Text != "tenant-a prompt" {
		t.Fatalf("GetPrompt = %#v, %v", prompt, err)
	}
}

func TestServerModeRequiresPolicyAndTenant(t *testing.T) {
	t.Parallel()
	resolver := TenantResolverFunc(func(*http.Request) (string, error) { return "tenant", nil })
	if _, err := NewServerMode(nil, nil, nil, resolver); err == nil {
		t.Fatal("nil policy accepted")
	}
	policy := ServerToolPolicyFunc(func(context.Context, ServerToolRequest) (agentsdk.ToolResult, error) {
		return agentsdk.ToolResult{}, nil
	})
	if _, err := NewServerMode(nil, nil, policy, nil); err == nil {
		t.Fatal("nil tenant resolver accepted")
	}
	mode, err := NewServerMode(nil, nil, policy, resolver)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxServerModeSessionsPerTenant; i++ {
		mode.sessions[fmt.Sprintf("session-%d", i)] = serverSessionBinding{tenant: "tenant", lastSeen: time.Now()}
	}
	if !mode.reserveSessionSlot("other-tenant") {
		t.Fatal("one tenant's quota blocked a different tenant")
	}
	mode.releaseSessionSlot("other-tenant")
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	mode.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("session capacity status = %d, want 429", response.Code)
	}
	if err := mode.Close(); err != nil {
		t.Fatal(err)
	}
	closedResponse := httptest.NewRecorder()
	mode.Handler().ServeHTTP(closedResponse, request)
	if closedResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed server status = %d, want 503", closedResponse.Code)
	}
}

type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for key, values := range t.header {
		clone.Header[key] = append([]string(nil), values...)
	}
	return t.base.RoundTrip(clone)
}
