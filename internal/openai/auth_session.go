package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type AuthMode string

const (
	AuthModeAPIKey AuthMode = "api-key"
	AuthModeOAuth  AuthMode = "oauth"
)

const (
	openAIBetaResponsesExperimental = "responses=experimental"
	oauthRefreshInterval            = 8 * 24 * time.Hour

	DefaultCodexClientVersion = "0.125.0"
	DefaultOAuthClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultOAuthIssuer        = "https://auth.openai.com"
	DefaultOAuthTokenEndpoint = "https://auth.openai.com/oauth/token"
)

// NormalizeAuthMode resolves configured mode and defaults to API key mode.
func NormalizeAuthMode(mode string) AuthMode {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case string(AuthModeOAuth):
		return AuthModeOAuth
	default:
		return AuthModeAPIKey
	}
}

type OpenAIAuthSession struct {
	mode   AuthMode
	apiKey string
	oauth  *oauthSessionState
	custom *customAuthSessionState
}

type CustomAuthSessionConfig struct {
	RequestHeaders  func(context.Context) (map[string]string, error)
	Refresh         func(context.Context) error
	SupportsRefresh func() bool
	SDKAPIKey       string
}

type OAuthSessionConfig struct {
	AuthJSON      []byte
	AuthJSONPath  string
	AccountID     string
	AccountIDPath string
	ClientID      string
	TokenEndpoint string
	ClientVersion string
	RefreshClient *http.Client
}

type oauthSessionState struct {
	mu sync.Mutex

	refreshGroup  singleflight.Group
	httpClient    *http.Client
	clientID      string
	tokenEndpoint string
	clientVersion string

	accessToken  string
	refreshToken string
	idToken      string
	accountID    string
	lastRefresh  time.Time

	refreshDisabled bool
}

type customAuthSessionState struct {
	refreshGroup singleflight.Group
	headers      func(context.Context) (map[string]string, error)
	refresh      func(context.Context) error
	supports     func() bool
	sdkAPIKey    string
}

type oauthAuthFile struct {
	Tokens      oauthAuthTokens `json:"tokens"`
	LastRefresh string          `json:"last_refresh"`
}

type oauthAuthTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type oauthRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
}

func NewAPIKeyAuthSession(apiKey string) *OpenAIAuthSession {
	return &OpenAIAuthSession{mode: AuthModeAPIKey, apiKey: strings.TrimSpace(apiKey)}
}

func NewCustomAuthSession(cfg CustomAuthSessionConfig) *OpenAIAuthSession {
	return &OpenAIAuthSession{
		mode: AuthModeAPIKey,
		custom: &customAuthSessionState{
			headers:   cfg.RequestHeaders,
			refresh:   cfg.Refresh,
			supports:  cfg.SupportsRefresh,
			sdkAPIKey: strings.TrimSpace(cfg.SDKAPIKey),
		},
	}
}

func CodexClientVersion() string {
	return DefaultCodexClientVersion
}

func NewOAuthAuthSessionFromFile(authJSONPath, accountIDPath string) (*OpenAIAuthSession, error) {
	return NewOAuthAuthSessionFromConfig(OAuthSessionConfig{
		AuthJSONPath:  authJSONPath,
		AccountIDPath: accountIDPath,
	})
}

func NewOAuthAuthSessionFromSecretData(authJSON []byte, accountIDOverride string) (*OpenAIAuthSession, error) {
	return NewOAuthAuthSessionFromConfig(OAuthSessionConfig{
		AuthJSON:  authJSON,
		AccountID: accountIDOverride,
	})
}

func NewOAuthAuthSessionFromConfig(cfg OAuthSessionConfig) (*OpenAIAuthSession, error) {
	authJSON := cfg.AuthJSON
	if len(authJSON) == 0 {
		authJSONPath := strings.TrimSpace(cfg.AuthJSONPath)
		if authJSONPath == "" {
			return nil, fmt.Errorf("oauth auth JSON path or data is required")
		}
		var err error
		authJSON, err = os.ReadFile(authJSONPath)
		if err != nil {
			return nil, fmt.Errorf("read oauth auth json: %w", err)
		}
	}

	accountIDOverride := strings.TrimSpace(cfg.AccountID)
	if accountIDOverride == "" {
		accountIDPath := strings.TrimSpace(cfg.AccountIDPath)
		if accountIDPath != "" {
			rawAccountID, err := os.ReadFile(accountIDPath)
			if err == nil {
				accountIDOverride = strings.TrimSpace(string(rawAccountID))
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read oauth account-id file: %w", err)
			}
		}
	}

	parsed := oauthAuthFile{}
	if err := json.Unmarshal(authJSON, &parsed); err != nil {
		return nil, fmt.Errorf("parse oauth auth json: %w", err)
	}

	accessToken := strings.TrimSpace(parsed.Tokens.AccessToken)
	refreshToken := strings.TrimSpace(parsed.Tokens.RefreshToken)
	idToken := strings.TrimSpace(parsed.Tokens.IDToken)
	accountID := strings.TrimSpace(accountIDOverride)
	if accountID == "" {
		accountID = strings.TrimSpace(parsed.Tokens.AccountID)
	}
	if accountID == "" {
		accountID = deriveAccountIDFromIDToken(idToken)
	}

	if accessToken == "" && refreshToken == "" {
		return nil, fmt.Errorf("oauth material is missing both access and refresh tokens")
	}
	if accountID == "" && refreshToken == "" {
		return nil, fmt.Errorf("oauth material is missing account_id and cannot derive it from id_token")
	}

	httpClient := cfg.RefreshClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	clientID := strings.TrimSpace(cfg.ClientID)
	if clientID == "" {
		clientID = DefaultOAuthClientID
	}
	tokenEndpoint := strings.TrimSpace(cfg.TokenEndpoint)
	if tokenEndpoint == "" {
		tokenEndpoint = DefaultOAuthTokenEndpoint
	}

	return &OpenAIAuthSession{
		mode: AuthModeOAuth,
		oauth: &oauthSessionState{
			httpClient:    httpClient,
			clientID:      clientID,
			tokenEndpoint: tokenEndpoint,
			clientVersion: strings.TrimSpace(cfg.ClientVersion),
			accessToken:   accessToken,
			refreshToken:  refreshToken,
			idToken:       idToken,
			accountID:     accountID,
			lastRefresh:   parseISOTime(parsed.LastRefresh),
		},
	}, nil
}

func (s *OpenAIAuthSession) RequestHeaders(ctx context.Context) (map[string]string, error) {
	if s == nil {
		return nil, fmt.Errorf("auth session is required")
	}
	if s.custom != nil {
		if s.custom.headers == nil {
			return nil, fmt.Errorf("custom auth session headers function is required")
		}
		return s.custom.headers(ctx)
	}
	switch s.mode {
	case AuthModeOAuth:
		if err := s.ensureOAuthAccessToken(ctx); err != nil {
			return nil, err
		}
		s.oauth.mu.Lock()
		defer s.oauth.mu.Unlock()
		return map[string]string{
			"Authorization":      "Bearer " + s.oauth.accessToken,
			"chatgpt-account-id": s.oauth.accountID,
			"OpenAI-Beta":        openAIBetaResponsesExperimental,
		}, nil
	default:
		apiKey := strings.TrimSpace(s.apiKey)
		if apiKey == "" {
			return nil, fmt.Errorf("openai api key is empty")
		}
		return map[string]string{"Authorization": "Bearer " + apiKey}, nil
	}
}

func (s *OpenAIAuthSession) SupportsRefresh() bool {
	if s != nil && s.custom != nil {
		return s.custom.supports != nil && s.custom.supports()
	}
	if s == nil || s.mode != AuthModeOAuth || s.oauth == nil {
		return false
	}
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	return !s.oauth.refreshDisabled && strings.TrimSpace(s.oauth.refreshToken) != ""
}

func (s *OpenAIAuthSession) IsOAuth() bool {
	return s != nil && s.mode == AuthModeOAuth
}

func (s *OpenAIAuthSession) DisableRefresh() {
	if s == nil || s.oauth == nil {
		return
	}
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	s.oauth.refreshDisabled = true
}

func (s *OpenAIAuthSession) Refresh(ctx context.Context) error {
	if s != nil && s.custom != nil {
		if !s.SupportsRefresh() || s.custom.refresh == nil {
			return nil
		}
		_, err, _ := s.custom.refreshGroup.Do("refresh", func() (any, error) {
			return nil, s.custom.refresh(ctx)
		})
		return err
	}
	if !s.SupportsRefresh() {
		return nil
	}
	_, err, _ := s.oauth.refreshGroup.Do("refresh", func() (any, error) {
		return nil, s.refreshOAuthToken(ctx)
	})
	return err
}

func (s *OpenAIAuthSession) ensureOAuthAccessToken(ctx context.Context) error {
	if s == nil || s.mode != AuthModeOAuth || s.oauth == nil {
		return nil
	}
	s.oauth.mu.Lock()
	accessToken := strings.TrimSpace(s.oauth.accessToken)
	refreshToken := strings.TrimSpace(s.oauth.refreshToken)
	lastRefresh := s.oauth.lastRefresh
	s.oauth.mu.Unlock()

	if shouldRefreshOAuthAccessToken(accessToken, lastRefresh, time.Now()) && refreshToken != "" {
		if err := s.Refresh(ctx); err != nil {
			return err
		}
	}

	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()

	if strings.TrimSpace(s.oauth.accessToken) == "" {
		return fmt.Errorf("oauth access token is unavailable")
	}
	if strings.TrimSpace(s.oauth.accountID) == "" {
		s.oauth.accountID = deriveAccountIDFromIDToken(s.oauth.idToken)
	}
	if strings.TrimSpace(s.oauth.accountID) == "" {
		return fmt.Errorf("chatgpt account id is unavailable")
	}
	return nil
}

func (s *OpenAIAuthSession) refreshOAuthToken(ctx context.Context) error {
	s.oauth.mu.Lock()
	refreshToken := strings.TrimSpace(s.oauth.refreshToken)
	tokenEndpoint := strings.TrimSpace(s.oauth.tokenEndpoint)
	clientID := strings.TrimSpace(s.oauth.clientID)
	accessToken := strings.TrimSpace(s.oauth.accessToken)
	s.oauth.mu.Unlock()

	if refreshToken == "" {
		return fmt.Errorf("oauth refresh token is unavailable")
	}
	if tokenEndpoint == "" {
		tokenEndpoint = DefaultOAuthTokenEndpoint
	}
	if clientID == "" {
		clientID = DefaultOAuthClientID
	}

	payload := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     clientID,
		"scope":         "openid profile email offline_access",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal oauth refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build oauth refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.oauth.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth refresh request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read oauth refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := sanitizeSecretSnippet(string(raw), refreshToken, accessToken)
		snippet = sanitizeLogBody(snippet)
		return fmt.Errorf("oauth refresh failed with status %d: %s", resp.StatusCode, snippet)
	}

	refresh := oauthRefreshResponse{}
	if err := json.Unmarshal(raw, &refresh); err != nil {
		return fmt.Errorf("parse oauth refresh response: %w", err)
	}
	refresh.AccessToken = strings.TrimSpace(refresh.AccessToken)
	refresh.IDToken = strings.TrimSpace(refresh.IDToken)
	refresh.RefreshToken = strings.TrimSpace(refresh.RefreshToken)
	if refresh.AccessToken == "" {
		return fmt.Errorf("oauth refresh response missing access_token")
	}

	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()

	s.oauth.accessToken = refresh.AccessToken
	if refresh.IDToken != "" {
		s.oauth.idToken = refresh.IDToken
	}
	if refresh.RefreshToken != "" {
		s.oauth.refreshToken = refresh.RefreshToken
	}
	if strings.TrimSpace(s.oauth.accountID) == "" {
		s.oauth.accountID = deriveAccountIDFromIDToken(s.oauth.idToken)
	}
	s.oauth.lastRefresh = time.Now()
	if strings.TrimSpace(s.oauth.accountID) == "" {
		return fmt.Errorf("oauth refresh succeeded but account id is unavailable")
	}
	return nil
}

func (s *OpenAIAuthSession) NeedsRefresh() bool {
	if s == nil || s.mode != AuthModeOAuth || s.oauth == nil {
		return false
	}
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	if strings.TrimSpace(s.oauth.refreshToken) == "" {
		return false
	}
	return shouldRefreshOAuthAccessToken(s.oauth.accessToken, s.oauth.lastRefresh, time.Now())
}

func (s *OpenAIAuthSession) RefreshAndSerialize(ctx context.Context, accountIDOverride string) ([]byte, error) {
	if s == nil || s.mode != AuthModeOAuth || s.oauth == nil {
		return nil, fmt.Errorf("oauth auth session is required")
	}
	if err := s.Refresh(ctx); err != nil {
		return nil, err
	}
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	accountID := strings.TrimSpace(accountIDOverride)
	if accountID == "" {
		accountID = s.oauth.accountID
	}
	authFile := oauthAuthFile{
		Tokens: oauthAuthTokens{
			IDToken:      s.oauth.idToken,
			AccessToken:  s.oauth.accessToken,
			RefreshToken: s.oauth.refreshToken,
			AccountID:    accountID,
		},
		LastRefresh: s.oauth.lastRefresh.UTC().Format(time.RFC3339Nano),
	}
	return json.Marshal(authFile)
}

func (s *OpenAIAuthSession) sdkAPIKey() string {
	if s == nil {
		return ""
	}
	if s.custom != nil {
		if s.custom.sdkAPIKey != "" {
			return s.custom.sdkAPIKey
		}
		return "custom-placeholder"
	}
	if s.mode == AuthModeOAuth {
		return "oauth-placeholder"
	}
	return strings.TrimSpace(s.apiKey)
}

func shouldRefreshOAuthAccessToken(accessToken string, lastRefresh time.Time, now time.Time) bool {
	if strings.TrimSpace(accessToken) == "" {
		return true
	}
	if !lastRefresh.IsZero() && (lastRefresh.Before(now.Add(-oauthRefreshInterval)) || lastRefresh.Equal(now.Add(-oauthRefreshInterval))) {
		return true
	}
	return false
}

func parseISOTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func accessTokenExpiry(accessToken string) (time.Time, bool) {
	claims := parseJWTClaims(accessToken)
	if claims == nil {
		return time.Time{}, false
	}
	expRaw, ok := claims["exp"]
	if !ok {
		return time.Time{}, false
	}
	switch value := expRaw.(type) {
	case float64:
		return time.Unix(int64(value), 0), true
	case json.Number:
		n, err := value.Int64()
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(n, 0), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(n, 0), true
	default:
		return time.Time{}, false
	}
}

func deriveAccountIDFromIDToken(idToken string) string {
	claims := parseJWTClaims(idToken)
	if claims == nil {
		return ""
	}
	authClaimRaw, ok := claims["https://api.openai.com/auth"]
	if !ok {
		return ""
	}
	authClaim, ok := authClaimRaw.(map[string]any)
	if !ok {
		return ""
	}
	accountID, _ := authClaim["chatgpt_account_id"].(string)
	return strings.TrimSpace(accountID)
}

func parseJWTClaims(token string) map[string]any {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	rawPayload := strings.TrimSpace(parts[1])
	if rawPayload == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(rawPayload)
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(rawPayload)
		if err != nil {
			return nil
		}
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func newAuthHTTPClient(session *OpenAIAuthSession, timeout time.Duration) *http.Client {
	baseTransport := newOpenAIBaseTransport()
	transport := http.RoundTripper(baseTransport)
	if session != nil {
		transport = &authRoundTripper{base: baseTransport, session: session}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

type authRoundTripper struct {
	base    http.RoundTripper
	session *OpenAIAuthSession
}

func (t *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.session == nil {
		return t.base.RoundTrip(req)
	}
	firstReq, err := cloneRequest(req)
	if err != nil {
		return nil, err
	}
	if err := applyAuthHeaders(firstReq, t.session); err != nil {
		return nil, err
	}
	maybeInjectCodexClientVersion(firstReq, t.session)
	forcedStream := maybeNormalizeCodexResponsesBody(firstReq, t.session)
	resp, err := t.base.RoundTrip(firstReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized || !t.session.SupportsRefresh() {
		return maybeFinalizeCodexResponse(firstReq, resp, forcedStream, t.session)
	}
	if err := t.session.Refresh(req.Context()); err != nil {
		return maybeFinalizeCodexResponse(firstReq, resp, forcedStream, t.session)
	}
	secondReq, err := cloneRequest(req)
	if err != nil {
		return resp, nil
	}
	if err := applyAuthHeaders(secondReq, t.session); err != nil {
		return resp, nil
	}
	maybeInjectCodexClientVersion(secondReq, t.session)
	forcedStream2 := maybeNormalizeCodexResponsesBody(secondReq, t.session)
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	resp2, err := t.base.RoundTrip(secondReq)
	if err != nil {
		return nil, err
	}
	return maybeFinalizeCodexResponse(secondReq, resp2, forcedStream2, t.session)
}

// maybeInjectCodexClientVersion appends the required client_version query
// parameter for ChatGPT backend API requests (OAuth mode to chatgpt.com).
func maybeInjectCodexClientVersion(req *http.Request, session *OpenAIAuthSession) {
	if session == nil || session.mode != AuthModeOAuth {
		return
	}
	if req.URL == nil || !isChatGPTBackendHost(req.URL.Host) {
		return
	}
	q := req.URL.Query()
	if q.Get("client_version") == "" {
		q.Set("client_version", sessionCodexClientVersion(session))
		req.URL.RawQuery = q.Encode()
	}
}

func sessionCodexClientVersion(session *OpenAIAuthSession) string {
	if session != nil && session.oauth != nil {
		session.oauth.mu.Lock()
		version := strings.TrimSpace(session.oauth.clientVersion)
		session.oauth.mu.Unlock()
		if version != "" {
			return version
		}
	}
	return DefaultCodexClientVersion
}

// maybeNormalizeCodexResponsesBody rewrites POST /responses bodies for
// the ChatGPT backend (chatgpt.com). The Codex endpoint rejects fields
// that the standard OpenAI API accepts:
//   - max_output_tokens must be removed
//   - store must be explicitly false
//   - stream must be true (backend rejects non-streaming)
//
// Returns true if stream was forced to true (caller must collect SSE).
func maybeNormalizeCodexResponsesBody(req *http.Request, session *OpenAIAuthSession) bool {
	if session == nil || session.mode != AuthModeOAuth {
		return false
	}
	if req.URL == nil || !isChatGPTBackendHost(req.URL.Host) {
		return false
	}
	if req.Method != http.MethodPost {
		return false
	}
	if !strings.HasSuffix(req.URL.Path, "/responses") {
		return false
	}
	if req.Body == nil {
		return false
	}
	data, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil || len(data) == 0 {
		req.Body = io.NopCloser(bytes.NewReader(data))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		return false
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		req.Body = io.NopCloser(bytes.NewReader(data))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		return false
	}

	delete(body, "max_output_tokens")
	// Codex manages its own output budget and does not use these standard
	// Responses params; opencode/codex-cli omit them too.
	delete(body, "truncation")
	delete(body, "prompt_cache_retention")
	// NOTE: do NOT strip "include". With store=false (required below) plus
	// reasoning, the Responses API requires include=["reasoning.encrypted_content"]
	// so the client receives the encrypted reasoning to round-trip on the next
	// turn; stripping it makes Codex reject the request. opencode and codex-cli
	// both send it.
	//
	// Codex rejects reasoning.effort values outside [low medium high max]
	// ("xhigh"/"minimal", which the standard API accepts), so remap those. It
	// DOES accept reasoning.summary (and emits summaries), so keep it.
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			if remapped := codexReasoningEffort(effort); remapped != effort {
				reasoning["effort"] = remapped
			}
		}
	}
	if _, ok := body["store"]; !ok {
		body["store"] = false
	}

	// Log request summary for diagnostics.
	var toolCount int
	if tools, ok := body["tools"].([]any); ok {
		toolCount = len(tools)
	}
	model, _ := body["model"].(string)
	hasInstructions := false
	if instr, ok := body["instructions"]; ok && instr != nil {
		if s, ok := instr.(string); ok && s != "" {
			hasInstructions = true
		}
	}
	log.Printf("[openai] codex request: model=%s tools=%d hasInstructions=%v url=%s",
		model, toolCount, hasInstructions, req.URL.String())

	// ChatGPT backend requires stream=true on /responses.
	// If the caller already set stream=true (real SDK streaming), we still
	// normalize the body but return false so the SSE stream passes through
	// to the SDK's stream decoder instead of being collected to JSON.
	callerSetStream := body["stream"] == true
	body["stream"] = true

	normalized, err := json.Marshal(body)
	if err != nil {
		req.Body = io.NopCloser(bytes.NewReader(data))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		return false
	}
	req.Body = io.NopCloser(bytes.NewReader(normalized))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(normalized)), nil }
	req.ContentLength = int64(len(normalized))
	return !callerSetStream
}

// codexReasoningEffort maps a reasoning effort label to the set the ChatGPT
// Codex backend accepts: [low medium high max]. The standard Responses API also
// accepts "minimal" and "xhigh", which the Codex backend rejects with a 400
// ("invalid_reasoning_effort"), so collapse those to the nearest supported tier.
func codexReasoningEffort(effort string) string {
	switch effort {
	case "xhigh":
		return "max"
	case "minimal":
		return "low"
	default:
		return effort
	}
}

func maybeFinalizeCodexResponse(req *http.Request, resp *http.Response, forcedStream bool, session *OpenAIAuthSession) (*http.Response, error) {
	logCodexErrorBody(req, resp, session)
	finalResp, err := maybeCollectSSE(resp, forcedStream)
	if err != nil {
		return nil, err
	}
	maybeNormalizeCodexCompactResponse(req, finalResp, session)
	return finalResp, nil
}

// logCodexErrorBody surfaces the response body for a failed Codex request so the
// exact rejection reason (e.g. "invalid_reasoning_effort", a missing required
// field) is visible in logs instead of an opaque "400 Bad Request". It reads and
// restores the body so downstream error handling still sees it.
func logCodexErrorBody(req *http.Request, resp *http.Response, session *OpenAIAuthSession) {
	if session == nil || session.mode != AuthModeOAuth || resp == nil || req == nil || req.URL == nil {
		return
	}
	if !isChatGPTBackendHost(req.URL.Host) || resp.StatusCode < 400 {
		return
	}
	if resp.Body == nil {
		log.Printf("[openai] codex error: status=%d url=%s (empty body)", resp.StatusCode, req.URL.Path)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return
	}
	log.Printf("[openai] codex error: status=%d url=%s body=%s", resp.StatusCode, req.URL.Path, strings.TrimSpace(string(body)))
}

// ChatGPT's Codex compact endpoint can return JSON without a JSON content type.
// Normalize only successful compact responses so provider errors stay visible.
func maybeNormalizeCodexCompactResponse(req *http.Request, resp *http.Response, session *OpenAIAuthSession) {
	if session == nil || session.mode != AuthModeOAuth {
		return
	}
	if req == nil || req.URL == nil || !isChatGPTBackendHost(req.URL.Host) {
		return
	}
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/responses/compact") {
		return
	}
	if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		resp.Header.Set("Content-Type", "application/json")
	}
}

// maybeCollectSSE reads an SSE stream response and returns a synthetic
// non-streaming JSON response containing the completed response object.
// This is needed because the ChatGPT backend requires stream=true on
// /responses, but our SDK caller expects a regular JSON response.
// If forcedStream is false, the response is returned as-is.
func maybeCollectSSE(resp *http.Response, forcedStream bool) (*http.Response, error) {
	if !forcedStream {
		return resp, nil
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	// We forced stream=true, so always collect regardless of Content-Type.
	// The backend may return text/event-stream, application/octet-stream,
	// or even an empty Content-Type.
	defer resp.Body.Close()
	return collectSSEToJSON(resp)
}

// collectSSEToJSON reads the full SSE stream and returns a synthetic HTTP
// response with the final response JSON body.
//
// It captures the response object from any SSE event that carries one (not
// just "response.completed"), keeping the latest. This is more robust against
// backend variations where the completed event may differ in format. Error
// events are logged for diagnostics.
func collectSSEToJSON(resp *http.Response) (*http.Response, error) {
	var completedData []byte
	var latestResponseData []byte
	var eventCount int
	var lastEventType string
	outputItems := map[int]*collectedSSEOutputItem{}

	buf := make([]byte, 0, 4096)
	remainder := make([]byte, 0, 512)

	// Read the stream in chunks and parse SSE events.
	tmp := make([]byte, 8192)
	totalRead := 0
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			totalRead += n
			if totalRead > maxProviderResponseBytes {
				return nil, fmt.Errorf("SSE stream exceeded %d byte cap", maxProviderResponseBytes)
			}
			buf = append(remainder, tmp[:n]...)
			remainder = remainder[:0]

			// Process complete lines.
			for {
				idx := bytes.IndexByte(buf, '\n')
				if idx < 0 {
					remainder = append(remainder, buf...)
					buf = buf[:0]
					break
				}
				line := buf[:idx]
				buf = buf[idx+1:]

				line = bytes.TrimRight(line, "\r")

				if bytes.HasPrefix(line, []byte("data: ")) {
					data := line[6:]
					if bytes.Equal(data, []byte("[DONE]")) {
						// Stream finished.
						goto done
					}
					eventCount++
					// Parse to check event type.
					var evt collectedSSEEvent
					if json.Unmarshal(data, &evt) == nil {
						lastEventType = evt.Type
						collectSSEOutputEvent(outputItems, evt)
						// Capture response.completed as the primary source.
						if evt.Type == "response.completed" && len(evt.Response) > 0 {
							completedData = []byte(evt.Response)
						}
						// Also capture the latest response from ANY event that
						// carries one, as a fallback. The openai-oauth reference
						// impl does this for robustness.
						if len(evt.Response) > 0 {
							latestResponseData = []byte(evt.Response)
						}
						// Log error events for diagnostics.
						if evt.Type == "error" {
							log.Printf("[openai] SSE error event: %s", string(data))
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return nil, fmt.Errorf("reading SSE stream: %w", readErr)
			}
			break
		}
	}

done:
	// Prefer response.completed data, fall back to latest response from any event.
	responseData := completedData
	if responseData == nil {
		responseData = latestResponseData
	}
	if responseData == nil {
		return nil, fmt.Errorf("SSE stream ended without response data (events=%d lastType=%s)", eventCount, lastEventType)
	}
	if completedData == nil && latestResponseData != nil {
		log.Printf("[openai] SSE: no response.completed event, using latest response from %d events (lastType=%s)", eventCount, lastEventType)
	}
	responseData = fillSSECollectedOutput(responseData, outputItems)

	synth := &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      resp.Proto,
		ProtoMajor: resp.ProtoMajor,
		ProtoMinor: resp.ProtoMinor,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(responseData)),
		Request:    resp.Request,
	}
	synth.Header.Set("Content-Type", "application/json")
	synth.ContentLength = int64(len(responseData))
	return synth, nil
}

type collectedSSEEvent struct {
	Type        string          `json:"type"`
	Response    json.RawMessage `json:"response"`
	OutputIndex *int            `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	Part        json.RawMessage `json:"part"`
	Delta       string          `json:"delta"`
	Text        string          `json:"text"`
	Arguments   string          `json:"arguments"`
}

type collectedSSEOutputItem struct {
	id     string
	typ    string
	role   string
	status string

	callID string
	name   string

	text      strings.Builder
	arguments strings.Builder
}

func collectSSEOutputEvent(items map[int]*collectedSSEOutputItem, evt collectedSSEEvent) {
	if evt.OutputIndex == nil {
		return
	}
	item := sseOutputItem(items, *evt.OutputIndex)

	switch evt.Type {
	case "response.output_item.added", "response.output_item.done":
		mergeSSEOutputItem(item, evt.Item)
	case "response.content_part.added", "response.content_part.done":
		mergeSSEContentPart(item, evt.Part)
	case "response.output_text.delta":
		item.ensureType("message")
		item.text.WriteString(evt.Delta)
	case "response.output_text.done":
		item.ensureType("message")
		if item.text.Len() == 0 && evt.Text != "" {
			item.text.WriteString(evt.Text)
		}
	case "response.function_call_arguments.delta":
		item.ensureType("function_call")
		item.arguments.WriteString(evt.Delta)
	case "response.function_call_arguments.done":
		item.ensureType("function_call")
		if item.arguments.Len() == 0 && evt.Arguments != "" {
			item.arguments.WriteString(evt.Arguments)
		}
	}
}

func sseOutputItem(items map[int]*collectedSSEOutputItem, index int) *collectedSSEOutputItem {
	item := items[index]
	if item == nil {
		item = &collectedSSEOutputItem{}
		items[index] = item
	}
	return item
}

func (item *collectedSSEOutputItem) ensureType(typ string) {
	if item != nil && item.typ == "" {
		item.typ = typ
	}
}

func mergeSSEOutputItem(item *collectedSSEOutputItem, raw json.RawMessage) {
	if item == nil || len(raw) == 0 {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	mergeSSECommonFields(item, obj)
	if item.typ == "message" {
		mergeSSEContentText(item, obj["content"])
	}
	if item.typ == "function_call" {
		if item.arguments.Len() == 0 {
			if args, _ := obj["arguments"].(string); args != "" {
				item.arguments.WriteString(args)
			}
		}
	}
}

func mergeSSEContentPart(item *collectedSSEOutputItem, raw json.RawMessage) {
	if item == nil || len(raw) == 0 {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	if typ, _ := obj["type"].(string); typ == "output_text" {
		item.ensureType("message")
		if item.text.Len() == 0 {
			if text, _ := obj["text"].(string); text != "" {
				item.text.WriteString(text)
			}
		}
	}
}

func mergeSSECommonFields(item *collectedSSEOutputItem, obj map[string]any) {
	if typ, _ := obj["type"].(string); typ != "" {
		item.typ = typ
	}
	if id, _ := obj["id"].(string); id != "" {
		item.id = id
	}
	if role, _ := obj["role"].(string); role != "" {
		item.role = role
	}
	if status, _ := obj["status"].(string); status != "" {
		item.status = status
	}
	if callID, _ := obj["call_id"].(string); callID != "" {
		item.callID = callID
	}
	if name, _ := obj["name"].(string); name != "" {
		item.name = name
	}
}

func mergeSSEContentText(item *collectedSSEOutputItem, raw any) {
	if item == nil || item.text.Len() != 0 {
		return
	}
	content, ok := raw.([]any)
	if !ok {
		return
	}
	for _, partRaw := range content {
		part, ok := partRaw.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := part["type"].(string); typ != "output_text" {
			continue
		}
		if text, _ := part["text"].(string); text != "" {
			item.text.WriteString(text)
		}
	}
}

func fillSSECollectedOutput(responseData []byte, items map[int]*collectedSSEOutputItem) []byte {
	if len(items) == 0 {
		return responseData
	}
	var response map[string]any
	if err := json.Unmarshal(responseData, &response); err != nil {
		return responseData
	}
	if !responseOutputIsEmpty(response["output"]) {
		return responseData
	}
	output := collectedSSEOutput(items)
	if len(output) == 0 {
		return responseData
	}
	response["output"] = output
	updated, err := json.Marshal(response)
	if err != nil {
		return responseData
	}
	return updated
}

func responseOutputIsEmpty(raw any) bool {
	if raw == nil {
		return true
	}
	output, ok := raw.([]any)
	return ok && len(output) == 0
}

func collectedSSEOutput(items map[int]*collectedSSEOutputItem) []any {
	indexes := make([]int, 0, len(items))
	for index := range items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	out := make([]any, 0, len(indexes))
	for _, index := range indexes {
		item := items[index]
		if item == nil {
			continue
		}
		switch {
		case item.typ == "function_call" || (item.name != "" && item.arguments.Len() > 0):
			call := map[string]any{
				"type":      "function_call",
				"arguments": item.arguments.String(),
			}
			if item.id != "" {
				call["id"] = item.id
			}
			if item.callID != "" {
				call["call_id"] = item.callID
			}
			if item.name != "" {
				call["name"] = item.name
			}
			if item.status != "" {
				call["status"] = item.status
			}
			out = append(out, call)
		case item.typ == "message" || item.text.Len() > 0:
			text := item.text.String()
			if text == "" {
				continue
			}
			role := item.role
			if role == "" {
				role = "assistant"
			}
			status := item.status
			if status == "" {
				status = "completed"
			}
			message := map[string]any{
				"type":   "message",
				"role":   role,
				"status": status,
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": text,
					},
				},
			}
			if item.id != "" {
				message["id"] = item.id
			}
			out = append(out, message)
		}
	}
	return out
}

// isChatGPTBackendHost reports whether the host belongs to the ChatGPT
// backend API.
func isChatGPTBackendHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "chatgpt.com" || strings.HasSuffix(h, ".chatgpt.com")
}

func applyAuthHeaders(req *http.Request, session *OpenAIAuthSession) error {
	headers, err := session.RequestHeaders(req.Context())
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return nil
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	cloned := req.Clone(req.Context())
	if req.Body == nil {
		return cloned, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("request body is not replayable")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	cloned.Body = body
	return cloned, nil
}

func sanitizeSecretSnippet(raw string, secrets ...string) string {
	out := raw
	for _, secret := range secrets {
		trimmed := strings.TrimSpace(secret)
		if trimmed == "" {
			continue
		}
		out = strings.ReplaceAll(out, trimmed, "[REDACTED]")
	}
	return out
}
