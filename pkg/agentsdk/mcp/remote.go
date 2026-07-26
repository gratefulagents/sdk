package mcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	webtools "github.com/gratefulagents/sdk/pkg/agentsdk/tools/web"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultRemoteMaxRequests      = 8
	defaultRemoteMaxResponseBytes = 8 << 20
)

// ErrOutcomeUnknown marks a remote operation whose response was lost after it
// may have reached the server. Callers must reconcile it and must not retry
// blindly.
var (
	ErrOutcomeUnknown   = errors.New("remote MCP outcome unknown")
	errRemoteNotSent    = errors.New("remote MCP request was not sent")
	errRemoteDefinitive = errors.New("remote MCP server returned a definitive HTTP error")
	errRemoteAmbiguous  = errors.New("remote MCP server returned an ambiguous HTTP error")
	errResponseTooLarge = errors.New("remote MCP response exceeds limit")
)

type remoteAttemptContextKey struct{}

type remoteAttemptState struct {
	ambiguousHTTP atomic.Bool
}

func remoteAttemptFromContext(ctx context.Context) *remoteAttemptState {
	state, _ := ctx.Value(remoteAttemptContextKey{}).(*remoteAttemptState)
	return state
}

// OutcomeUnknownError identifies the immutable operation requiring
// reconciliation without retaining transport errors or credentials.
type OutcomeUnknownError struct {
	Server string
	Tool   string
}

func (e *OutcomeUnknownError) Error() string {
	return fmt.Sprintf("%v for MCP server %q tool %q; request was not replayed", ErrOutcomeUnknown, e.Server, e.Tool)
}

func (e *OutcomeUnknownError) Unwrap() error { return ErrOutcomeUnknown }

// HeaderProvider supplies request headers from a host-controlled credential
// store. It is called for every request, allowing safe token refresh without
// putting credentials in .mcp.json or retaining them in a connection pool.
// Implementations must isolate credentials by tenantID and serverName.
type HeaderProvider interface {
	Headers(context.Context, string, string, *url.URL) (http.Header, error)
}

// HeaderProviderFunc adapts a function to HeaderProvider.
type HeaderProviderFunc func(context.Context, string, string, *url.URL) (http.Header, error)

func (f HeaderProviderFunc) Headers(ctx context.Context, tenantID, serverName string, endpoint *url.URL) (http.Header, error) {
	return f(ctx, tenantID, serverName, endpoint)
}

// OAuthToken is a bearer token plus the claims the provider validated while
// acquiring it. The manager independently checks audience, required scopes,
// and expiry before putting the credential on the wire.
type OAuthToken struct {
	AccessToken string
	Audience    string
	Scopes      []string
	Expiry      time.Time
}

// OAuthTokenProvider obtains tenant-isolated access tokens. It is called for
// every HTTP request so providers can refresh tokens without sharing them
// between managers or tenants.
type OAuthTokenProvider interface {
	Token(context.Context, string, string) (OAuthToken, error)
}

// OAuthTokenProviderFunc adapts a function to OAuthTokenProvider.
type OAuthTokenProviderFunc func(context.Context, string, string) (OAuthToken, error)

func (f OAuthTokenProviderFunc) Token(ctx context.Context, tenantID, serverName string) (OAuthToken, error) {
	return f(ctx, tenantID, serverName)
}

// RemoteAuditEvent records credential-free remote lifecycle and operation
// provenance.
type RemoteAuditEvent struct {
	TenantID  string
	Server    string
	Operation string
	Outcome   string
}

// RemoteAuditHook persists remote audit events. Returning an error fails the
// operation closed before network execution where possible.
type RemoteAuditHook interface {
	RecordRemoteMCP(context.Context, RemoteAuditEvent) error
}

// RemoteAuditHookFunc adapts a function to RemoteAuditHook.
type RemoteAuditHookFunc func(context.Context, RemoteAuditEvent) error

func (f RemoteAuditHookFunc) RecordRemoteMCP(ctx context.Context, event RemoteAuditEvent) error {
	return f(ctx, event)
}

type remoteOAuthPolicy struct {
	provider       OAuthTokenProvider
	audience       string
	requiredScopes map[string]struct{}
}

type remoteManagerOptions struct {
	enabledServers   map[string]struct{}
	privateServers   map[string]struct{}
	readOnlyTools    map[string]map[string]struct{}
	tenantID         string
	headers          HeaderProvider
	oauthByServer    map[string]remoteOAuthPolicy
	rootCAs          *x509.CertPool
	maxRequests      int
	maxResponseBytes int64
	audit            RemoteAuditHook
}

// WithRemoteServers enables network MCP for names approved by trusted host
// policy. A repository-controlled .mcp.json cannot enable remote access alone.
func WithRemoteServers(names ...string) ManagerOption {
	return func(opts *managerOptions) {
		if opts.remote.enabledServers == nil {
			opts.remote.enabledServers = make(map[string]struct{})
		}
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				opts.remote.enabledServers[name] = struct{}{}
			}
		}
	}
}

// WithRemoteReadOnlyTools classifies exact raw tool names as read-only in
// trusted host policy. A server annotation and repository trust flag are also
// required; an empty host list exposes no remote tools.
func WithRemoteReadOnlyTools(serverName string, toolNames ...string) ManagerOption {
	return func(opts *managerOptions) {
		serverName = strings.TrimSpace(serverName)
		if serverName == "" {
			return
		}
		if opts.remote.readOnlyTools == nil {
			opts.remote.readOnlyTools = make(map[string]map[string]struct{})
		}
		allowed := opts.remote.readOnlyTools[serverName]
		if allowed == nil {
			allowed = make(map[string]struct{})
			opts.remote.readOnlyTools[serverName] = allowed
		}
		for _, toolName := range toolNames {
			if toolName = strings.TrimSpace(toolName); toolName != "" {
				allowed[toolName] = struct{}{}
			}
		}
	}
}

func remoteToolAllowedByHost(opts managerOptions, serverName, toolName string) bool {
	tools := opts.remote.readOnlyTools[serverName]
	_, ok := tools[strings.TrimSpace(toolName)]
	return ok
}

// WithPrivateNetworkRemoteServers permits the named, already-enabled servers
// to resolve to private addresses and use plain HTTP. This is intended for
// explicitly trusted intranet endpoints and tests; public access stays the
// fail-closed default.
func WithPrivateNetworkRemoteServers(names ...string) ManagerOption {
	return func(opts *managerOptions) {
		if opts.remote.privateServers == nil {
			opts.remote.privateServers = make(map[string]struct{})
		}
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				opts.remote.privateServers[name] = struct{}{}
			}
		}
	}
}

// WithRemoteTenant binds credentials and connections to one non-empty tenant.
func WithRemoteTenant(tenantID string) ManagerOption {
	return func(opts *managerOptions) { opts.remote.tenantID = strings.TrimSpace(tenantID) }
}

// WithRemoteHeaderProvider installs a host-controlled per-tenant header source.
func WithRemoteHeaderProvider(provider HeaderProvider) ManagerOption {
	return func(opts *managerOptions) { opts.remote.headers = provider }
}

// WithRemoteOAuth installs a bearer-token provider and immutable expected
// claims. Tokens failing audience, scope, or expiry checks never reach the
// transport.
func WithRemoteOAuth(serverName string, provider OAuthTokenProvider, audience string, scopes ...string) ManagerOption {
	return func(opts *managerOptions) {
		serverName = strings.TrimSpace(serverName)
		if serverName == "" {
			return
		}
		if opts.remote.oauthByServer == nil {
			opts.remote.oauthByServer = make(map[string]remoteOAuthPolicy)
		}
		policy := remoteOAuthPolicy{provider: provider, audience: strings.TrimSpace(audience), requiredScopes: make(map[string]struct{}, len(scopes))}
		for _, scope := range scopes {
			if scope = strings.TrimSpace(scope); scope != "" {
				policy.requiredScopes[scope] = struct{}{}
			}
		}
		opts.remote.oauthByServer[serverName] = policy
	}
}

// WithRemoteRootCAs configures trusted roots without disabling TLS
// verification. The pool is cloned to prevent mutation after construction.
func WithRemoteRootCAs(roots *x509.CertPool) ManagerOption {
	return func(opts *managerOptions) {
		if roots != nil {
			opts.remote.rootCAs = roots.Clone()
		}
	}
}

// WithRemoteMaxConcurrentRequests bounds in-flight requests per remote server.
func WithRemoteMaxConcurrentRequests(max int) ManagerOption {
	return func(opts *managerOptions) {
		if max > 0 {
			opts.remote.maxRequests = max
		}
	}
}

// WithRemoteMaxResponseBytes bounds each HTTP response body.
func WithRemoteMaxResponseBytes(max int64) ManagerOption {
	return func(opts *managerOptions) {
		if max > 0 {
			opts.remote.maxResponseBytes = max
		}
	}
}

// WithRemoteAuditHook installs a credential-free audit sink.
func WithRemoteAuditHook(hook RemoteAuditHook) ManagerOption {
	return func(opts *managerOptions) { opts.remote.audit = hook }
}

func auditRemote(ctx context.Context, opts managerOptions, server, operation, outcome string) error {
	if opts.remote.audit == nil {
		return nil
	}
	return opts.remote.audit.RecordRemoteMCP(ctx, RemoteAuditEvent{
		TenantID:  opts.remote.tenantID,
		Server:    server,
		Operation: operation,
		Outcome:   outcome,
	})
}

func connectRemoteServer(ctx context.Context, name string, cfg ServerConfig, opts managerOptions) (*serverConn, error) {
	if _, ok := opts.remote.enabledServers[name]; !ok {
		return nil, fmt.Errorf("MCP server %q: remote transport is not enabled by host policy", name)
	}
	if err := auditRemote(ctx, opts, name, "connect", "attempted"); err != nil {
		return nil, fmt.Errorf("MCP server %q: remote audit unavailable", name)
	}
	private := false
	if _, ok := opts.remote.privateServers[name]; ok {
		private = true
	}
	endpoint, err := validateRemoteEndpoint(ctx, cfg.URL, private)
	if err != nil {
		return nil, fmt.Errorf("MCP server %q: invalid remote endpoint: %v", name, err)
	}
	oauthPolicy := opts.remote.oauthByServer[name]
	if (opts.remote.headers != nil || oauthPolicy.provider != nil) && opts.remote.tenantID == "" {
		return nil, fmt.Errorf("MCP server %q: authenticated remote transport requires a tenant ID", name)
	}

	client, cleanup, reflections := newRemoteHTTPClient(name, endpoint, private, opts.remote)
	var transport mcpsdk.Transport
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case TransportLegacySSE:
		transport = &mcpsdk.SSEClientTransport{Endpoint: endpoint.String(), HTTPClient: client}
	default:
		transport = &mcpsdk.StreamableClientTransport{
			Endpoint:             endpoint.String(),
			HTTPClient:           client,
			MaxRetries:           -1,
			DisableStandaloneSSE: true,
		}
	}

	connectCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		connectCtx, cancel = context.WithTimeout(ctx, defaultConnectTimeout)
	}
	defer cancel()
	mcpClient := mcpsdk.NewClient(&mcpsdk.Implementation{Name: clientName, Version: clientVersion}, nil)
	session, err := mcpClient.Connect(connectCtx, transport, nil)
	if err != nil {
		cleanup()
		// Deliberately omit endpoint and wrapped transport error: either may
		// contain query parameters, credentials, or provider diagnostics.
		return nil, fmt.Errorf("MCP server %q: remote connection failed", name)
	}
	if err := auditRemote(ctx, opts, name, "connect", "completed"); err != nil {
		cleanup()
		_ = session.Close()
		return nil, fmt.Errorf("MCP server %q: remote audit unavailable", name)
	}
	conn := &serverConn{
		name:         name,
		client:       mcpClient,
		session:      session,
		capabilities: session.InitializeResult().Capabilities,
		cfg:          cfg,
		remote:       true,
		cleanup:      cleanup,
		reflections:  reflections,
	}
	conn.reconnect = func(reconnectCtx context.Context) (*serverConn, error) {
		return connectRemoteServer(reconnectCtx, name, cfg, opts)
	}
	return conn, nil
}

func validateRemoteEndpoint(ctx context.Context, raw string, allowPrivate bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("URL is malformed")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("query strings and fragments are not allowed")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("embedded credentials are not allowed")
	}
	if !allowPrivate && parsed.Scheme != "https" {
		return nil, fmt.Errorf("HTTPS is required")
	}
	validated, err := webtools.ValidateHTTPURL(ctx, parsed.String(), webtools.URLSecurityOptions{AllowPrivateNetworkURLs: allowPrivate})
	if err != nil {
		return nil, fmt.Errorf("URL is blocked by network policy")
	}
	return validated, nil
}

func newRemoteHTTPClient(serverName string, endpoint *url.URL, allowPrivate bool, opts remoteManagerOptions) (*http.Client, func(), *credentialReflections) {
	client := webtools.NewSafeHTTPClientWithOptions(0, webtools.URLSecurityOptions{AllowPrivateNetworkURLs: allowPrivate})
	transport := client.Transport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = defaultConnectTimeout
	lifecycle, cancel := context.WithCancel(context.Background())
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: opts.rootCAs}
	max := opts.maxRequests
	if max <= 0 {
		max = defaultRemoteMaxRequests
	}
	maxResponseBytes := opts.maxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultRemoteMaxResponseBytes
	}
	authPolicy := opts.oauthByServer[serverName]
	reflections := &credentialReflections{}
	client.Transport = &remoteRoundTripper{
		base:             transport,
		serverName:       serverName,
		endpoint:         endpoint,
		tenantID:         opts.tenantID,
		headers:          opts.headers,
		oauth:            authPolicy.provider,
		audience:         authPolicy.audience,
		scopes:           authPolicy.requiredScopes,
		semaphore:        make(chan struct{}, max),
		lifecycle:        lifecycle,
		allowPrivate:     allowPrivate,
		reflections:      reflections,
		maxResponseBytes: maxResponseBytes,
	}
	// Redirects are rejected rather than replaying tenant credentials to a new
	// origin. Reconfiguration must be explicit and auditable.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("MCP redirects are disabled")
	}
	return client, func() {
		cancel()
		transport.CloseIdleConnections()
		reflections.Clear()
	}, reflections
}

type remoteRoundTripper struct {
	base             http.RoundTripper
	serverName       string
	endpoint         *url.URL
	tenantID         string
	headers          HeaderProvider
	oauth            OAuthTokenProvider
	audience         string
	scopes           map[string]struct{}
	semaphore        chan struct{}
	lifecycle        context.Context
	allowPrivate     bool
	reflections      *credentialReflections
	maxResponseBytes int64
}

func (r *remoteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.endpoint == nil || req.URL == nil || !strings.EqualFold(req.URL.Scheme, r.endpoint.Scheme) || !strings.EqualFold(req.URL.Host, r.endpoint.Host) {
		return nil, fmt.Errorf("%w: endpoint origin changed", errRemoteNotSent)
	}
	if _, err := webtools.ValidateHTTPURL(req.Context(), req.URL.String(), webtools.URLSecurityOptions{AllowPrivateNetworkURLs: r.allowPrivate}); err != nil {
		return nil, fmt.Errorf("%w: endpoint blocked by network policy", errRemoteNotSent)
	}
	lifecycle := r.lifecycle
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	select {
	case r.semaphore <- struct{}{}:
	case <-req.Context().Done():
		return nil, fmt.Errorf("%w: %w", errRemoteNotSent, req.Context().Err())
	case <-lifecycle.Done():
		return nil, fmt.Errorf("%w: connection closed", errRemoteNotSent)
	}
	release := func() { <-r.semaphore }
	baseRequestCtx := req.Context()
	longLivedSSE := req.Method == http.MethodGet && strings.Contains(req.Header.Get("Accept"), "text/event-stream")
	if longLivedSSE {
		// Legacy SSE owns this long-lived response after Connect returns. The
		// transport lifecycle cancels it; the handshake remains bounded by
		// caller cancellation and ResponseHeaderTimeout.
		baseRequestCtx = context.WithoutCancel(baseRequestCtx)
	}
	requestCtx, cancelRequest := context.WithCancel(baseRequestCtx)
	stopCaller := func() bool { return false }
	if longLivedSSE {
		stopCaller = context.AfterFunc(req.Context(), cancelRequest)
	}
	stopLifecycle := context.AfterFunc(lifecycle, cancelRequest)
	cleanupRequest := func() {
		stopCaller()
		stopLifecycle()
		cancelRequest()
	}

	clone := req.Clone(requestCtx)
	clone.GetBody = nil
	clone.Header = req.Header.Clone()
	if clone.Header.Get("Idempotency-Key") != "" || clone.Header.Get("X-Idempotency-Key") != "" {
		cleanupRequest()
		release()
		return nil, fmt.Errorf("%w: idempotency headers are forbidden", errRemoteNotSent)
	}
	var secrets [][]byte
	if r.headers != nil {
		headers, err := r.headers.Headers(req.Context(), r.tenantID, r.serverName, clone.URL)
		if err != nil {
			cleanupRequest()
			release()
			return nil, fmt.Errorf("%w: credential provider failed", errRemoteNotSent)
		}
		headerValues := 0
		for key, values := range headers {
			headerValues += len(values)
			if headerValues > 32 {
				cleanupRequest()
				release()
				return nil, fmt.Errorf("%w: credential provider returned too many values", errRemoteNotSent)
			}
			if !allowedCredentialHeader(key) {
				cleanupRequest()
				release()
				return nil, fmt.Errorf("%w: credential provider returned a forbidden header", errRemoteNotSent)
			}
			clone.Header.Del(key)
			for _, value := range values {
				if len(value) > 16*1024 {
					cleanupRequest()
					release()
					return nil, fmt.Errorf("%w: credential header value is too large", errRemoteNotSent)
				}
				clone.Header.Add(key, value)
				secrets = appendSecretPatterns(secrets, value)
			}
		}
	}
	if r.oauth != nil {
		token, err := r.oauth.Token(req.Context(), r.tenantID, r.serverName)
		if err != nil || !validOAuthToken(token, r.audience, r.scopes, time.Now()) {
			cleanupRequest()
			release()
			return nil, fmt.Errorf("%w: OAuth token validation failed", errRemoteNotSent)
		}
		clone.Header.Set("Authorization", "Bearer "+token.AccessToken)
		secrets = appendSecretPatterns(secrets, token.AccessToken)
		secrets = appendSecretPatterns(secrets, "Bearer "+token.AccessToken)
	}
	if r.reflections != nil {
		r.reflections.Add(secrets)
	}
	resp, err := r.base.RoundTrip(clone)
	if err == nil && longLivedSSE {
		stopCaller()
	}
	if err != nil {
		cleanupRequest()
		release()
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError {
		_ = resp.Body.Close()
		cleanupRequest()
		release()
		return nil, errRemoteDefinitive
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		if attempt := remoteAttemptFromContext(req.Context()); attempt != nil {
			attempt.ambiguousHTTP.Store(true)
		}
		_ = resp.Body.Close()
		cleanupRequest()
		release()
		return nil, errRemoteAmbiguous
	}
	maxResponseBytes := r.maxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultRemoteMaxResponseBytes
	}
	body := io.ReadCloser(&maxBytesReadCloser{inner: resp.Body, remaining: maxResponseBytes})
	body = &cleanupReadCloser{ReadCloser: body, cleanup: cleanupRequest}
	resp.Body = &releaseOnClose{ReadCloser: body, release: release}
	return resp, nil
}

func appendSecretPatterns(patterns [][]byte, value string) [][]byte {
	if value == "" {
		return patterns
	}
	patterns = append(patterns, []byte(value))
	if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
		escaped := encoded[1 : len(encoded)-1]
		if !bytes.Equal(escaped, []byte(value)) {
			patterns = append(patterns, append([]byte(nil), escaped...))
		}
	}
	return patterns
}

func allowedCredentialHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "host", "content-length", "connection", "transfer-encoding", "proxy-authorization", "cookie", "accept", "content-type", "user-agent", "origin", "referer", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "x-original-url", "x-rewrite-url", "idempotency-key", "x-idempotency-key", "mcp-session-id", "mcp-protocol-version":
		return false
	default:
		return true
	}
}

func validOAuthToken(token OAuthToken, audience string, required map[string]struct{}, now time.Time) bool {
	if strings.TrimSpace(token.AccessToken) == "" || strings.TrimSpace(audience) == "" || token.Audience != audience {
		return false
	}
	if token.Expiry.IsZero() || !token.Expiry.After(now.Add(30*time.Second)) {
		return false
	}
	actual := make(map[string]struct{}, len(token.Scopes))
	for _, scope := range token.Scopes {
		actual[strings.TrimSpace(scope)] = struct{}{}
	}
	for scope := range required {
		if _, ok := actual[scope]; !ok {
			return false
		}
	}
	return true
}

type maxBytesReadCloser struct {
	inner     io.ReadCloser
	remaining int64
}

func (r *maxBytesReadCloser) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.inner.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, errResponseTooLarge
	}
	return n, err
}

func (r *maxBytesReadCloser) Close() error { return r.inner.Close() }

type cleanupReadCloser struct {
	io.ReadCloser
	cleanup func()
	once    sync.Once
}

func (r *cleanupReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil {
		r.once.Do(r.cleanup)
	}
	return n, err
}

func (r *cleanupReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.cleanup)
	return err
}

type credentialReflections struct {
	mu       sync.RWMutex
	patterns [][]byte
}

func (r *credentialReflections) Add(patterns [][]byte) {
	if r == nil || len(patterns) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pattern := range patterns {
		if len(pattern) > 0 {
			r.patterns = append(r.patterns, append([]byte(nil), pattern...))
		}
	}
	if len(r.patterns) > 64 {
		drop := len(r.patterns) - 64
		for i := 0; i < drop; i++ {
			for j := range r.patterns[i] {
				r.patterns[i][j] = 0
			}
		}
		r.patterns = append([][]byte(nil), r.patterns[drop:]...)
	}
}

func (r *credentialReflections) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	for i := range r.patterns {
		for j := range r.patterns[i] {
			r.patterns[i][j] = 0
		}
	}
	r.patterns = nil
	r.mu.Unlock()
}

func (r *credentialReflections) Contains(value any) bool {
	if r == nil || value == nil {
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, pattern := range r.patterns {
		if bytes.Contains(encoded, pattern) {
			return true
		}
	}
	return false
}

type releaseOnClose struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *releaseOnClose) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *releaseOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
