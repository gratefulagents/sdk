package mcp

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type remoteEchoArgs struct {
	Text string `json:"text"`
}

func remoteEcho(_ context.Context, _ *mcpsdk.CallToolRequest, args remoteEchoArgs) (*mcpsdk.CallToolResult, any, error) {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: args.Text}}, StructuredContent: map[string]any{"echo": args.Text}}, nil, nil
}

func newRemoteTestHandler(requiredHeader string) http.Handler {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "remote-test", Version: "1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "read",
		Description: "read data",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, remoteEcho)
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "write",
		Description: "mutate data",
	}, remoteEcho)
	server.AddResource(&mcpsdk.Resource{URI: "memory://item/1", Name: "item"}, func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: "memory://item/1", Text: "resource"}}}, nil
	})
	server.AddPrompt(&mcpsdk.Prompt{Name: "hello"}, func(context.Context, *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
		return &mcpsdk.GetPromptResult{Messages: []*mcpsdk.PromptMessage{{Role: mcpsdk.Role("user"), Content: &mcpsdk.TextContent{Text: "hello"}}}}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requiredHeader != "" && r.Header.Get("X-API-Key") != requiredHeader {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func TestRemoteStreamableHTTPReadOnlyAuthenticated(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(newRemoteTestHandler("tenant-a-secret"))
	defer server.Close()

	var mu sync.Mutex
	var tenants []string
	var auditEvents []RemoteAuditEvent
	provider := HeaderProviderFunc(func(_ context.Context, tenant, server string, _ *url.URL) (http.Header, error) {
		mu.Lock()
		tenants = append(tenants, tenant+"/"+server)
		mu.Unlock()
		return http.Header{"X-API-Key": []string{"tenant-a-secret"}}, nil
	})
	audit := RemoteAuditHookFunc(func(_ context.Context, event RemoteAuditEvent) error {
		mu.Lock()
		auditEvents = append(auditEvents, event)
		mu.Unlock()
		return nil
	})
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"api": {
			Type:              "streamable-http",
			URL:               server.URL,
			TrustReadOnlyHint: true,
		},
	}}, WithRemoteServers("api"), WithRemoteReadOnlyTools("api", "read"), WithPrivateNetworkRemoteServers("api"), WithRemoteTenant("tenant-a"), WithRemoteHeaderProvider(provider), WithRemoteAuditHook(audit))
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	defer manager.Close()

	descriptors := manager.ToolDescriptors()
	if len(descriptors) != 1 || descriptors[0].ToolName != "read" || !descriptors[0].ReadOnly {
		t.Fatalf("remote descriptors = %#v, want only read-only tool", descriptors)
	}
	result, err := manager.CallTool(t.Context(), descriptors[0].QualifiedName, map[string]any{"text": "ok"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || result.StructuredContent == nil {
		t.Fatalf("tool result = %#v, want text and structured content", result)
	}
	if _, err := manager.CallTool(t.Context(), descriptors[0].QualifiedName, map[string]any{"text": "tenant-a-secret"}); err == nil || !strings.Contains(err.Error(), "credential material") {
		t.Fatalf("credential reflection error = %v", err)
	}
	if _, err := manager.CallTool(t.Context(), BuildToolName("api", "write"), nil); err == nil || !strings.Contains(err.Error(), "unknown MCP tool") {
		t.Fatalf("mutating remote call error = %v, want unknown tool", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, got := range tenants {
		if got != "tenant-a/api" {
			t.Fatalf("provider tenant/server = %q, want tenant-a/api", got)
		}
	}
	var attempted, completed bool
	for _, event := range auditEvents {
		if event.TenantID != "tenant-a" || event.Server != "api" {
			t.Fatalf("audit provenance = %#v", event)
		}
		attempted = attempted || (event.Operation == "tools/call:read" && event.Outcome == "attempted")
		completed = completed || (event.Operation == "tools/call:read" && event.Outcome == "completed")
	}
	if !attempted || !completed {
		t.Fatalf("missing tool audit events: %#v", auditEvents)
	}
}

func TestRemoteStreamableHTTPOAuth(t *testing.T) {
	t.Parallel()
	handler := newRemoteTestHandler("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer oauth-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, request)
	}))
	defer server.Close()
	provider := OAuthTokenProviderFunc(func(context.Context, string, string) (OAuthToken, error) {
		return OAuthToken{AccessToken: "oauth-secret", Audience: "mcp-api", Scopes: []string{"read"}, Expiry: time.Now().Add(time.Hour)}, nil
	})
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"oauth": {Type: TransportStreamableHTTP, URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("oauth"), WithRemoteReadOnlyTools("oauth", "read"), WithPrivateNetworkRemoteServers("oauth"), WithRemoteTenant("tenant-a"), WithRemoteOAuth("oauth", provider, "mcp-api", "read"))
	if err != nil {
		t.Fatalf("OAuth manager: %v", err)
	}
	defer manager.Close()
	if _, err := manager.CallTool(t.Context(), BuildToolName("oauth", "read"), map[string]any{"text": "ok"}); err != nil {
		t.Fatalf("OAuth CallTool: %v", err)
	}
}

func TestRemoteStreamableHTTPUnauthenticated(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(newRemoteTestHandler(""))
	defer server.Close()
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"public": {Type: "streamable-http", URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("public"), WithRemoteReadOnlyTools("public", "read"), WithPrivateNetworkRemoteServers("public"))
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	defer manager.Close()
	if len(manager.ToolDescriptors()) != 1 {
		t.Fatalf("tool descriptors = %d, want 1", len(manager.ToolDescriptors()))
	}
	resources, err := manager.ListResources(t.Context(), "public")
	if err != nil || len(resources) != 1 || resources[0].URI != "memory://item/1" {
		t.Fatalf("ListResources = %#v, %v", resources, err)
	}
	prompts, err := manager.ListPrompts(t.Context(), "public")
	if err != nil || len(prompts) != 1 || prompts[0].Name != "hello" {
		t.Fatalf("ListPrompts = %#v, %v", prompts, err)
	}
	prompt, err := manager.GetPrompt(t.Context(), "public", "hello", nil)
	if err != nil || len(prompt.Messages) != 1 {
		t.Fatalf("GetPrompt = %#v, %v", prompt, err)
	}
	manager.InvalidateDiscovery("public")
	if len(manager.resourceCache) != 0 || len(manager.promptCache) != 0 {
		t.Fatal("InvalidateDiscovery did not clear caches")
	}
}

func TestRemoteLegacySSECompatibility(t *testing.T) {
	t.Parallel()
	protocolServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sse-test", Version: "1"}, nil)
	mcpsdk.AddTool(protocolServer, &mcpsdk.Tool{Name: "read", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, remoteEcho)
	server := httptest.NewServer(mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return protocolServer }, nil))
	defer server.Close()
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"legacy": {Type: TransportLegacySSE, URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("legacy"), WithRemoteReadOnlyTools("legacy", "read"), WithPrivateNetworkRemoteServers("legacy"))
	if err != nil {
		t.Fatalf("legacy SSE manager: %v", err)
	}
	defer manager.Close()
	if _, err := manager.CallTool(t.Context(), BuildToolName("legacy", "read"), map[string]any{"text": "ok"}); err != nil {
		t.Fatalf("legacy SSE CallTool: %v", err)
	}
}

func TestRemoteHintAloneDoesNotExposeTool(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(newRemoteTestHandler(""))
	defer server.Close()
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"untrusted": {Type: TransportStreamableHTTP, URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("untrusted"), WithPrivateNetworkRemoteServers("untrusted"))
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	defer manager.Close()
	if got := manager.ToolDescriptors(); len(got) != 0 {
		t.Fatalf("server hint exposed tools without host classification: %#v", got)
	}
}

func TestRemotePolicyFailsClosed(t *testing.T) {
	t.Parallel()
	cfg := Config{MCPServers: map[string]ServerConfig{
		"remote": {Type: "streamable-http", URL: "https://127.0.0.1/mcp", TrustReadOnlyHint: true},
	}}
	for _, test := range []struct {
		name string
		opts []ManagerOption
		want string
	}{
		{name: "host opt-in required", want: "not enabled by host policy"},
		{name: "private address blocked", opts: []ManagerOption{WithRemoteServers("remote")}, want: "blocked by network policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), cfg, test.opts...)
			if manager != nil {
				manager.Close()
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAuthenticatedRemoteRequiresTenantBeforeNetwork(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	provider := HeaderProviderFunc(func(context.Context, string, string, *url.URL) (http.Header, error) {
		return http.Header{"Authorization": []string{"Bearer secret"}}, nil
	})
	_, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"auth": {Type: TransportStreamableHTTP, URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("auth"), WithPrivateNetworkRemoteServers("auth"), WithRemoteHeaderProvider(provider))
	if err == nil || !strings.Contains(err.Error(), "requires a tenant ID") {
		t.Fatalf("error = %v, want missing tenant failure", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("network calls = %d, want 0", calls.Load())
	}
}

func TestRemoteTLSVerificationAndCustomRoots(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(newRemoteTestHandler(""))
	defer server.Close()
	cfg := Config{MCPServers: map[string]ServerConfig{
		"tls": {Type: "streamable-http", URL: server.URL, TrustReadOnlyHint: true},
	}}
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), cfg, WithRemoteServers("tls"), WithPrivateNetworkRemoteServers("tls"))
	if manager != nil {
		manager.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "remote connection failed") {
		t.Fatalf("invalid TLS error = %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	manager, err = NewManagerFromConfig(t.Context(), t.TempDir(), cfg, WithRemoteServers("tls"), WithPrivateNetworkRemoteServers("tls"), WithRemoteRootCAs(roots))
	if err != nil {
		t.Fatalf("custom trust roots: %v", err)
	}
	manager.Close()
}

func TestRemoteRedirectRejectedWithoutCredentialLeak(t *testing.T) {
	t.Parallel()
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()

	secret := "do-not-print-this-token"
	provider := HeaderProviderFunc(func(context.Context, string, string, *url.URL) (http.Header, error) {
		return nil, errors.New(secret)
	})
	_, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"redirect": {Type: "streamable-http", URL: redirect.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("redirect"), WithPrivateNetworkRemoteServers("redirect"), WithRemoteTenant("t"), WithRemoteHeaderProvider(provider))
	if err == nil {
		t.Fatal("redirect connection unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("credential leaked in error: %v", err)
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target calls = %d, want 0", targetCalls)
	}
}

func TestRemoteTransportPinsOriginBeforeCredentials(t *testing.T) {
	t.Parallel()
	endpoint, _ := url.Parse("https://example.com/mcp")
	var providerCalls atomic.Int32
	transport := &remoteRoundTripper{
		base:      roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("network was reached"); return nil, nil }),
		endpoint:  endpoint,
		semaphore: make(chan struct{}, 1),
		headers: HeaderProviderFunc(func(context.Context, string, string, *url.URL) (http.Header, error) {
			providerCalls.Add(1)
			return http.Header{"Authorization": []string{"Bearer secret"}}, nil
		}),
	}
	request, _ := http.NewRequest(http.MethodPost, "http://attacker.example/mcp", nil)
	_, err := transport.RoundTrip(request)
	if !errors.Is(err, errRemoteNotSent) || providerCalls.Load() != 0 {
		t.Fatalf("cross-origin error/provider calls = %v/%d", err, providerCalls.Load())
	}
}

func TestRemoteTransportDisablesHTTPReplayAndIdempotencyHeaders(t *testing.T) {
	t.Parallel()
	endpoint, _ := url.Parse("http://127.0.0.1/mcp")
	var baseCalls atomic.Int32
	transport := &remoteRoundTripper{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			baseCalls.Add(1)
			if request.GetBody != nil {
				t.Fatal("rewindable request body reached HTTP transport")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
		}),
		endpoint: endpoint, allowPrivate: true, semaphore: make(chan struct{}, 1),
	}
	request := remoteTestRequest(t.Context())
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("body")), nil }
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if baseCalls.Load() != 1 {
		t.Fatalf("base calls = %d, want 1", baseCalls.Load())
	}

	transport.headers = HeaderProviderFunc(func(context.Context, string, string, *url.URL) (http.Header, error) {
		return http.Header{"Idempotency-Key": []string{"unsafe"}}, nil
	})
	_, err = transport.RoundTrip(remoteTestRequest(t.Context()))
	if !errors.Is(err, errRemoteNotSent) || baseCalls.Load() != 1 {
		t.Fatalf("idempotency header error/base calls = %v/%d", err, baseCalls.Load())
	}
}

func TestRemoteHTTP5xxIsOutcomeUnknownAndNeverReplayed(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	protocolServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "http-error", Version: "1"}, nil)
	mcpsdk.AddTool(protocolServer, &mcpsdk.Tool{Name: "read", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, remoteEcho)
	protocol := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return protocolServer }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			body, _ := io.ReadAll(request.Body)
			request.Body = io.NopCloser(strings.NewReader(string(body)))
			if strings.Contains(string(body), `"method":"tools/call"`) {
				toolCalls.Add(1)
				http.Error(w, "upstream response lost", http.StatusBadGateway)
				return
			}
		}
		protocol.ServeHTTP(w, request)
	}))
	defer server.Close()
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"http-error": {Type: TransportStreamableHTTP, URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("http-error"), WithRemoteReadOnlyTools("http-error", "read"), WithPrivateNetworkRemoteServers("http-error"))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	_, err = manager.CallTool(t.Context(), BuildToolName("http-error", "read"), map[string]any{"text": "ok"})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("5xx CallTool error = %v, want ErrOutcomeUnknown", err)
	}
	if toolCalls.Load() != 1 {
		t.Fatalf("5xx tools/call count = %d, want 1", toolCalls.Load())
	}
}

func TestReconnectedResourceUsesFreshCredentialReflectionState(t *testing.T) {
	t.Parallel()
	const secret = "tenant-resource-secret"
	provider := HeaderProviderFunc(func(context.Context, string, string, *url.URL) (http.Header, error) {
		return http.Header{"X-API-Key": []string{secret}}, nil
	})
	firstServer := httptest.NewServer(newRemoteTestHandler(secret))
	defer firstServer.Close()
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"resource": {Type: TransportStreamableHTTP, URL: firstServer.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("resource"), WithRemoteReadOnlyTools("resource", "read"), WithPrivateNetworkRemoteServers("resource"), WithRemoteTenant("tenant-a"), WithRemoteHeaderProvider(provider))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	freshProtocol := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fresh", Version: "1"}, nil)
	freshProtocol.AddResource(&mcpsdk.Resource{URI: "memory://secret", Name: "secret"}, func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: "memory://secret", Text: secret}}}, nil
	})
	freshHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return freshProtocol }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true})
	freshServer := httptest.NewServer(freshHandler)
	defer freshServer.Close()
	freshConfig := ServerConfig{Type: TransportStreamableHTTP, URL: freshServer.URL, TrustReadOnlyHint: true}
	fresh, err := connectRemoteServer(t.Context(), "resource", freshConfig, manager.opts)
	if err != nil {
		t.Fatal(err)
	}
	old := manager.servers["resource"]
	old.reconnect = func(context.Context) (*serverConn, error) { return fresh, nil }
	if err := old.session.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = manager.ReadResource(t.Context(), "resource", "memory://secret")
	if err == nil || !strings.Contains(err.Error(), "credential material") {
		t.Fatalf("reconnected resource reflection error = %v", err)
	}
}

func TestRemoteAuditFailsClosedBeforeResourceRequest(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	var failAudit atomic.Bool
	handler := newRemoteTestHandler("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, request)
	}))
	defer server.Close()
	audit := RemoteAuditHookFunc(func(context.Context, RemoteAuditEvent) error {
		if failAudit.Load() {
			return errors.New("audit unavailable")
		}
		return nil
	})
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"audit": {Type: TransportStreamableHTTP, URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("audit"), WithRemoteReadOnlyTools("audit", "read"), WithPrivateNetworkRemoteServers("audit"), WithRemoteAuditHook(audit))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	before := requests.Load()
	failAudit.Store(true)
	_, err = manager.ListResources(t.Context(), "audit")
	if err == nil || !strings.Contains(err.Error(), "audit unavailable") {
		t.Fatalf("ListResources audit error = %v", err)
	}
	if got := requests.Load(); got != before {
		t.Fatalf("resource request reached network: before=%d after=%d", before, got)
	}
}

func TestRemoteCancellationIsOutcomeUnknownAndNeverReplayed(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	protocolServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "cancel-test", Version: "1"}, nil)
	mcpsdk.AddTool(protocolServer, &mcpsdk.Tool{Name: "slow", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ map[string]any) (*mcpsdk.CallToolResult, any, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return protocolServer }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true})
	server := httptest.NewServer(handler)
	defer server.Close()
	manager, err := NewManagerFromConfig(t.Context(), t.TempDir(), Config{MCPServers: map[string]ServerConfig{
		"cancel": {Type: TransportStreamableHTTP, URL: server.URL, TrustReadOnlyHint: true},
	}}, WithRemoteServers("cancel"), WithRemoteReadOnlyTools("cancel", "slow"), WithPrivateNetworkRemoteServers("cancel"))
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err = manager.CallTool(ctx, BuildToolName("cancel", "slow"), nil)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("CallTool error = %v, want ErrOutcomeUnknown", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("tools/call count = %d, want exactly 1", calls.Load())
	}
}

func TestCredentialReflectionDetection(t *testing.T) {
	t.Parallel()
	reflections := &credentialReflections{}
	reflections.Add(appendSecretPatterns(nil, `tenant-"secret`))
	if !reflections.Contains(map[string]any{"content": `before tenant-"secret after`}) {
		t.Fatal("credential reflection was not detected")
	}
	if reflections.Contains(map[string]any{"content": "safe"}) {
		t.Fatal("safe content was treated as a credential reflection")
	}
}

func TestRemoteResponseSizeIsBounded(t *testing.T) {
	t.Parallel()
	body := &maxBytesReadCloser{inner: io.NopCloser(strings.NewReader("1234")), remaining: 3}
	_, err := io.ReadAll(body)
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestReconnectCannotResurrectClosedManager(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var freshCleanups atomic.Int32
	current := &serverConn{name: "remote", remote: true}
	current.reconnect = func(context.Context) (*serverConn, error) {
		close(started)
		<-release
		return &serverConn{name: "remote", remote: true, cleanup: func() { freshCleanups.Add(1) }}, nil
	}
	manager := &Manager{servers: map[string]*serverConn{"remote": current}, reconnects: make(map[string]*reconnectState), resourceCache: make(map[string]resourceCacheEntry), promptCache: make(map[string]promptCacheEntry)}
	done := make(chan error, 1)
	go func() {
		_, err := manager.reconnectServer(context.Background(), "remote", current)
		done <- err
	}()
	<-started
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("reconnect unexpectedly succeeded after Close")
	}
	if freshCleanups.Load() != 1 || len(manager.ConnectedServerNames()) != 0 {
		t.Fatalf("fresh cleanups/servers = %d/%v", freshCleanups.Load(), manager.ConnectedServerNames())
	}
}

func TestRemoteBackpressureIsBoundedAndCancellable(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	endpoint, _ := url.Parse("http://127.0.0.1/mcp")
	transport := &remoteRoundTripper{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			close(started)
			<-release
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
		}),
		endpoint:     endpoint,
		allowPrivate: true,
		semaphore:    make(chan struct{}, 1),
	}
	firstDone := make(chan error, 1)
	go func() {
		response, err := transport.RoundTrip(remoteTestRequest(context.Background()))
		if err == nil {
			response.Body.Close()
		}
		firstDone <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := transport.RoundTrip(remoteTestRequest(ctx))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second request error = %v, want deadline exceeded", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first request: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func remoteTestRequest(ctx context.Context) *http.Request {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/mcp", nil)
	return request
}

func TestOAuthTokenValidation(t *testing.T) {
	t.Parallel()
	now := time.Now()
	valid := OAuthToken{AccessToken: "secret", Audience: "mcp-api", Scopes: []string{"read", "profile"}, Expiry: now.Add(time.Hour)}
	if !validOAuthToken(valid, "mcp-api", map[string]struct{}{"read": {}}, now) {
		t.Fatal("valid token rejected")
	}
	for _, token := range []OAuthToken{
		{AccessToken: "secret", Audience: "other", Scopes: []string{"read"}, Expiry: now.Add(time.Hour)},
		{AccessToken: "secret", Audience: "mcp-api", Scopes: []string{"profile"}, Expiry: now.Add(time.Hour)},
		{AccessToken: "secret", Audience: "mcp-api", Scopes: []string{"read"}, Expiry: now.Add(time.Second)},
	} {
		if validOAuthToken(token, "mcp-api", map[string]struct{}{"read": {}}, now) {
			t.Fatalf("invalid token accepted: %#v", token)
		}
	}
}
