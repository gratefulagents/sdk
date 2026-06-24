package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewOAuthAuthSessionFromConfigReturnsErrorWhenAuthMaterialIsMissing(t *testing.T) {
	_, err := NewOAuthAuthSessionFromConfig(OAuthSessionConfig{})
	if err == nil {
		t.Fatal("NewOAuthAuthSessionFromConfig() error = nil, want non-nil")
	}
}

func TestNewOAuthAuthSessionFromConfigUsesAccountIDPath(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/auth.json"
	accountIDPath := dir + "/account-id"
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}
	if err := os.WriteFile(accountIDPath, []byte("acct-from-path\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(account-id) error = %v", err)
	}

	session, err := NewOAuthAuthSessionFromConfig(OAuthSessionConfig{
		AuthJSONPath:  authPath,
		AccountIDPath: accountIDPath,
	})
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromConfig() error = %v", err)
	}
	headers, err := session.RequestHeaders(context.Background())
	if err != nil {
		t.Fatalf("RequestHeaders() error = %v", err)
	}
	if got := headers["chatgpt-account-id"]; got != "acct-from-path" {
		t.Fatalf("chatgpt-account-id = %q, want %q", got, "acct-from-path")
	}
}

func TestNewOAuthAuthSessionFromConfigPrefersAccountIDOverPath(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/auth.json"
	accountIDPath := dir + "/account-id"
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh","account_id":"acct-from-payload"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}
	if err := os.WriteFile(accountIDPath, []byte("acct-from-path\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(account-id) error = %v", err)
	}

	session, err := NewOAuthAuthSessionFromConfig(OAuthSessionConfig{
		AuthJSONPath:  authPath,
		AccountID:     "acct-from-config",
		AccountIDPath: accountIDPath,
	})
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromConfig() error = %v", err)
	}
	headers, err := session.RequestHeaders(context.Background())
	if err != nil {
		t.Fatalf("RequestHeaders() error = %v", err)
	}
	if got := headers["chatgpt-account-id"]; got != "acct-from-config" {
		t.Fatalf("chatgpt-account-id = %q, want %q", got, "acct-from-config")
	}
}

func TestNewOAuthAuthSessionFromConfigFallsBackToPayloadAccountID(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/auth.json"
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh","account_id":"acct-from-payload"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}

	session, err := NewOAuthAuthSessionFromConfig(OAuthSessionConfig{
		AuthJSONPath:  authPath,
		AccountIDPath: dir + "/missing-account-id",
	})
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromConfig() error = %v", err)
	}
	headers, err := session.RequestHeaders(context.Background())
	if err != nil {
		t.Fatalf("RequestHeaders() error = %v", err)
	}
	if got := headers["chatgpt-account-id"]; got != "acct-from-payload" {
		t.Fatalf("chatgpt-account-id = %q, want %q", got, "acct-from-payload")
	}
}

func TestNewOAuthAuthSessionFromConfigReturnsErrorWhenAccountIDPathReadFails(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/auth.json"
	accountIDDir := dir + "/account-id-dir"
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}
	if err := os.Mkdir(accountIDDir, 0o755); err != nil {
		t.Fatalf("Mkdir(account-id-dir) error = %v", err)
	}

	_, err := NewOAuthAuthSessionFromConfig(OAuthSessionConfig{
		AuthJSONPath:  authPath,
		AccountIDPath: accountIDDir,
	})
	if err == nil {
		t.Fatal("NewOAuthAuthSessionFromConfig() error = nil, want non-nil")
	}
}

func TestNewOAuthAuthSessionFromConfigLeavesRefreshEnabledByDefault(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/auth.json"
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh","account_id":"acct-1"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}

	session, err := NewOAuthAuthSessionFromFile(authPath, "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromFile() error = %v", err)
	}
	if !session.SupportsRefresh() {
		t.Fatal("SupportsRefresh() = false, want true")
	}
}

func TestDisableRefreshDisablesOAuthRefresh(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/auth.json"
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh","account_id":"acct-1"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}

	session, err := NewOAuthAuthSessionFromFile(authPath, "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromFile() error = %v", err)
	}
	session.DisableRefresh()
	if session.SupportsRefresh() {
		t.Fatal("SupportsRefresh() = true, want false")
	}
}

func TestAPIKeyAuthSessionRequestHeaders(t *testing.T) {
	session := NewAPIKeyAuthSession("sk-test")
	headers, err := session.RequestHeaders(context.Background())
	if err != nil {
		t.Fatalf("RequestHeaders() error = %v", err)
	}
	if got := headers["Authorization"]; got != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer sk-test")
	}
}

func TestOAuthAuthSessionRequestHeadersIncludesAccountAndBeta(t *testing.T) {
	session, err := NewOAuthAuthSessionFromSecretData([]byte(`{
		"tokens":{
			"access_token":"access-1",
			"refresh_token":"refresh-1",
			"account_id":"acct-1"
		},
		"last_refresh":"2099-01-01T00:00:00Z"
	}`), "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromSecretData() error = %v", err)
	}

	headers, err := session.RequestHeaders(context.Background())
	if err != nil {
		t.Fatalf("RequestHeaders() error = %v", err)
	}
	if got := headers["Authorization"]; got != "Bearer access-1" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer access-1")
	}
	if got := headers["chatgpt-account-id"]; got != "acct-1" {
		t.Fatalf("chatgpt-account-id = %q, want %q", got, "acct-1")
	}
	if got := headers["OpenAI-Beta"]; got != openAIBetaResponsesExperimental {
		t.Fatalf("OpenAI-Beta = %q, want %q", got, openAIBetaResponsesExperimental)
	}
}

func TestOAuthAuthSessionRefreshSingleflight(t *testing.T) {
	var refreshCalls atomic.Int32
	var scopes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		rawBody, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		var payload map[string]any
		_ = json.Unmarshal(rawBody, &payload)
		if scope, _ := payload["scope"].(string); scope != "" {
			scopes = append(scopes, scope)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-1","refresh_token":"refresh-1","id_token":"` + jwtWithAccountID("acct-1", 4102444800) + `"}`))
	}))
	defer server.Close()

	session, err := NewOAuthAuthSessionFromSecretData([]byte(`{
		"tokens":{"refresh_token":"refresh-1"},
		"last_refresh":"2000-01-01T00:00:00Z"
	}`), "acct-1")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromSecretData() error = %v", err)
	}
	session.oauth.tokenEndpoint = server.URL + "/token"

	const workers = 20
	wg := sync.WaitGroup{}
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			headers, err := session.RequestHeaders(context.Background())
			if err != nil {
				t.Errorf("RequestHeaders() error = %v", err)
				return
			}
			if got := headers["Authorization"]; got != "Bearer fresh-1" {
				t.Errorf("Authorization = %q, want %q", got, "Bearer fresh-1")
			}
		}()
	}
	wg.Wait()

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh call count = %d, want 1", got)
	}
	if len(scopes) != 1 || scopes[0] != "openid profile email offline_access" {
		t.Fatalf("refresh scope payload = %v, want [openid profile email offline_access]", scopes)
	}
}

func TestOAuthAuthSessionRefreshAndSerializeUsesAccountIDOverride(t *testing.T) {
	session, err := NewOAuthAuthSessionFromSecretData([]byte(`{
		"tokens":{
			"access_token":"access-1",
			"account_id":"acct-original"
		},
		"last_refresh":"2099-01-01T00:00:00Z"
	}`), "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromSecretData() error = %v", err)
	}

	data, err := session.RefreshAndSerialize(context.Background(), "acct-override")
	if err != nil {
		t.Fatalf("RefreshAndSerialize() error = %v", err)
	}

	var parsed oauthAuthFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if parsed.Tokens.AccountID != "acct-override" {
		t.Fatalf("serialized account_id = %q, want acct-override", parsed.Tokens.AccountID)
	}
}

func TestAPIKeyAuthSessionRefreshAndSerializeReturnsError(t *testing.T) {
	_, err := NewAPIKeyAuthSession("sk-test").RefreshAndSerialize(context.Background(), "")
	if err == nil {
		t.Fatal("RefreshAndSerialize() error = nil, want non-nil for API-key session")
	}
}

func TestAuthRoundTripperRetriesAfter401WithRefresh(t *testing.T) {
	var chatCalls atomic.Int32
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat":
			chatCalls.Add(1)
			auth := r.Header.Get("Authorization")
			if auth == "Bearer stale-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if auth != "Bearer fresh-token" {
				http.Error(w, "bad auth", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh-token","refresh_token":"refresh-1","expires_in":3600}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	session, err := NewOAuthAuthSessionFromSecretData([]byte(`{
		"tokens":{
			"access_token":"stale-token",
			"refresh_token":"refresh-1",
			"account_id":"acct-1"
		},
		"last_refresh":"2099-01-01T00:00:00Z"
	}`), "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromSecretData() error = %v", err)
	}
	session.oauth.tokenEndpoint = server.URL + "/token"

	client := newAuthHTTPClient(session, 5*time.Second)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/chat", bytes.NewReader([]byte(`{"hello":"world"}`)))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Fatalf("chat call count = %d, want 2", got)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh call count = %d, want 1", got)
	}
}

func TestAuthHTTPClientUsesConfiguredCABundle(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	bundlePath := t.TempDir() + "/ca.pem"
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(bundlePath, certPEM, 0o600); err != nil {
		t.Fatalf("WriteFile(ca.pem) error = %v", err)
	}
	t.Setenv(gratefulCABundleEnv, bundlePath)
	t.Setenv("SSL_CERT_FILE", "")

	client := newAuthHTTPClient(nil, 5*time.Second)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestOAuthRefreshErrorRedactsSecret(t *testing.T) {
	const refreshSecret = "refresh-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refresh failed for "+refreshSecret, http.StatusUnauthorized)
	}))
	defer server.Close()

	session, err := NewOAuthAuthSessionFromSecretData([]byte(`{
		"tokens":{"refresh_token":"`+refreshSecret+`"},
		"last_refresh":"2000-01-01T00:00:00Z"
	}`), "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromSecretData() error = %v", err)
	}
	session.oauth.tokenEndpoint = server.URL

	err = session.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh() error = nil, want non-nil")
	}
	if strings.Contains(err.Error(), refreshSecret) {
		t.Fatalf("Refresh() error leaked secret token: %v", err)
	}
}

func jwtWithAccountID(accountID string, expUnix int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID + `"},"exp":` + strconv.FormatInt(expUnix, 10) + `}`))
	return header + "." + payload + "."
}

func TestMaybeNormalizeCodexResponsesBody(t *testing.T) {
	oauthSession := &OpenAIAuthSession{mode: AuthModeOAuth}
	apiKeySession := &OpenAIAuthSession{mode: AuthModeAPIKey}

	makeReq := func(host, path, body string) *http.Request {
		req, _ := http.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	readBody := func(req *http.Request) map[string]any {
		data, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		return m
	}

	t.Run("strips max_output_tokens for chatgpt backend", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses",
			`{"model":"gpt-5.4","max_output_tokens":4096,"instructions":"hi"}`)
		forced := maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		if _, ok := body["max_output_tokens"]; ok {
			t.Error("max_output_tokens should be removed")
		}
		if body["store"] != false {
			t.Error("store should default to false")
		}
		if body["model"] != "gpt-5.4" {
			t.Error("model should be preserved")
		}
		if body["stream"] != true {
			t.Error("stream should be forced to true")
		}
		if !forced {
			t.Error("should return true when stream was forced")
		}
	})

	t.Run("no-op for api.openai.com", func(t *testing.T) {
		req := makeReq("api.openai.com", "/v1/responses",
			`{"model":"gpt-4","max_output_tokens":4096}`)
		forced := maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		if _, ok := body["max_output_tokens"]; !ok {
			t.Error("max_output_tokens should NOT be removed for standard API")
		}
		if forced {
			t.Error("should return false for non-chatgpt host")
		}
	})

	t.Run("no-op for api-key session on chatgpt host", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses",
			`{"model":"gpt-4","max_output_tokens":4096}`)
		forced := maybeNormalizeCodexResponsesBody(req, apiKeySession)
		body := readBody(req)
		if _, ok := body["max_output_tokens"]; !ok {
			t.Error("max_output_tokens should NOT be removed for api-key mode")
		}
		if forced {
			t.Error("should return false for api-key mode")
		}
	})

	t.Run("preserves explicit store=true", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses",
			`{"model":"gpt-5.4","store":true}`)
		maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		if body["store"] != true {
			t.Errorf("explicit store=true should be preserved, got %v", body["store"])
		}
	})

	t.Run("no-op for non-responses path", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/models",
			`{"max_output_tokens":4096}`)
		forced := maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		if _, ok := body["max_output_tokens"]; !ok {
			t.Error("should not modify non-responses requests")
		}
		if forced {
			t.Error("should return false for non-responses path")
		}
	})

	t.Run("does not collect SSE when caller already set stream=true", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses",
			`{"model":"gpt-5.4","stream":true,"max_output_tokens":4096}`)
		forced := maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		if _, ok := body["max_output_tokens"]; ok {
			t.Error("max_output_tokens should still be removed")
		}
		if body["stream"] != true {
			t.Error("stream should remain true")
		}
		if forced {
			t.Error("should return false when caller already set stream=true (real streaming)")
		}
	})

	t.Run("strips api-only params but keeps include for chatgpt backend", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses",
			`{"model":"gpt-5.4","truncation":"auto","prompt_cache_retention":"24h","include":["reasoning.encrypted_content"]}`)
		maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		for _, key := range []string{"truncation", "prompt_cache_retention"} {
			if _, ok := body[key]; ok {
				t.Errorf("%s should be removed for ChatGPT backend", key)
			}
		}
		// include MUST be preserved: store=false + reasoning requires
		// include=[reasoning.encrypted_content] or Codex rejects the request.
		inc, ok := body["include"].([]any)
		if !ok || len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
			t.Errorf("include must be preserved for Codex, got %v", body["include"])
		}
	})

	t.Run("keeps reasoning.summary and effort for chatgpt backend", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses",
			`{"model":"gpt-5.3-codex","reasoning":{"effort":"high","summary":"auto"}}`)
		maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		reasoning, ok := body["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning object should be preserved, got %v", body["reasoning"])
		}
		// Codex accepts reasoning.summary (and emits summaries); keep it.
		if reasoning["summary"] != "auto" {
			t.Errorf("reasoning.summary should be kept for Codex, got %v", reasoning["summary"])
		}
		if reasoning["effort"] != "high" {
			t.Errorf("reasoning.effort must be preserved, got %v", reasoning["effort"])
		}
	})

	t.Run("passes reasoning effort through unchanged for chatgpt backend", func(t *testing.T) {
		// OpenAI models accept [none minimal low medium high xhigh]. "max" is the
		// Anthropic/Claude vocabulary and is never sent here, so no remap is done.
		for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
			req := makeReq("chatgpt.com", "/backend-api/codex/responses",
				`{"model":"gpt-5.4-mini","reasoning":{"effort":"`+effort+`"}}`)
			maybeNormalizeCodexResponsesBody(req, oauthSession)
			body := readBody(req)
			reasoning, _ := body["reasoning"].(map[string]any)
			if reasoning == nil || reasoning["effort"] != effort {
				t.Errorf("effort %q -> %v, want unchanged (OpenAI accepts xhigh, not max)", effort, reasoning["effort"])
			}
		}
	})

	t.Run("does not remap reasoning effort for standard api", func(t *testing.T) {
		req := makeReq("api.openai.com", "/v1/responses",
			`{"model":"gpt-5.4","reasoning":{"effort":"xhigh"}}`)
		maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning == nil || reasoning["effort"] != "xhigh" {
			t.Errorf("standard API must keep effort=xhigh, got %v", body["reasoning"])
		}
	})

	t.Run("preserves reasoning.summary for standard api", func(t *testing.T) {
		req := makeReq("api.openai.com", "/v1/responses",
			`{"model":"gpt-5.4","reasoning":{"effort":"high","summary":"auto"}}`)
		maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		reasoning, ok := body["reasoning"].(map[string]any)
		if !ok || reasoning["summary"] != "auto" {
			t.Errorf("reasoning.summary should be preserved for standard API, got %v", body["reasoning"])
		}
	})

	t.Run("collects SSE when caller did not set stream", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses",
			`{"model":"gpt-5.4","instructions":"hi"}`)
		forced := maybeNormalizeCodexResponsesBody(req, oauthSession)
		body := readBody(req)
		if body["stream"] != true {
			t.Error("stream should be forced to true")
		}
		if !forced {
			t.Error("should return true when stream was forced by normalizer")
		}
	})
}

func TestMaybeNormalizeCodexCompactResponse(t *testing.T) {
	oauthSession := &OpenAIAuthSession{mode: AuthModeOAuth}
	apiKeySession := &OpenAIAuthSession{mode: AuthModeAPIKey}

	makeReq := func(host, path string) *http.Request {
		req, _ := http.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(`{}`))
		return req
	}

	t.Run("adds json content type for chatgpt compact success", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses/compact")
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_compact","output":[]}`)),
		}
		maybeNormalizeCodexCompactResponse(req, resp, oauthSession)
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	})

	t.Run("normalizes octet-stream compact success", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses/compact")
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_compact","output":[]}`)),
		}
		maybeNormalizeCodexCompactResponse(req, resp, oauthSession)
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	})

	t.Run("does not touch non-chatgpt or api-key responses", func(t *testing.T) {
		req := makeReq("api.openai.com", "/v1/responses/compact")
		resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
		maybeNormalizeCodexCompactResponse(req, resp, oauthSession)
		if got := resp.Header.Get("Content-Type"); got != "" {
			t.Fatalf("standard API Content-Type = %q, want empty", got)
		}

		req = makeReq("chatgpt.com", "/backend-api/codex/responses/compact")
		maybeNormalizeCodexCompactResponse(req, resp, apiKeySession)
		if got := resp.Header.Get("Content-Type"); got != "" {
			t.Fatalf("api-key Content-Type = %q, want empty", got)
		}
	})

	t.Run("does not mask error responses", func(t *testing.T) {
		req := makeReq("chatgpt.com", "/backend-api/codex/responses/compact")
		resp := &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header)}
		maybeNormalizeCodexCompactResponse(req, resp, oauthSession)
		if got := resp.Header.Get("Content-Type"); got != "" {
			t.Fatalf("error Content-Type = %q, want empty", got)
		}
	})
}

func TestAuthRoundTripperNormalizesCodexCompactResponse(t *testing.T) {
	session, err := NewOAuthAuthSessionFromSecretData([]byte(`{
		"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh","account_id":"acct-1"},
		"last_refresh":"2099-01-01T00:00:00Z"
	}`), "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromSecretData() error = %v", err)
	}

	rt := &authRoundTripper{
		session: session,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.URL.Query().Get("client_version"); got != DefaultCodexClientVersion {
				t.Fatalf("client_version = %q, want %q", got, DefaultCodexClientVersion)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer oauth-access" {
				t.Fatalf("Authorization = %q, want OAuth bearer", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id":"resp_compact","output":[]}`)),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses/compact", strings.NewReader(`{"model":"gpt-5.5","input":[]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestCollectSSEToJSON(t *testing.T) {
	t.Run("extracts response.completed from SSE stream", func(t *testing.T) {
		sseBody := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_123","status":"in_progress"}}`,
			"",
			`data: {"type":"response.output_item.added","output_index":0}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_123","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello"}]}]}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")

		resp := &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseBody)),
		}

		result, err := collectSSEToJSON(resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		body, _ := io.ReadAll(result.Body)
		if result.StatusCode != 200 {
			t.Errorf("expected 200, got %d", result.StatusCode)
		}
		if result.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", result.Header.Get("Content-Type"))
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("response body is not valid JSON: %s", string(body))
		}
		if parsed["id"] != "resp_123" {
			t.Errorf("expected id=resp_123, got %v", parsed["id"])
		}
		if parsed["status"] != "completed" {
			t.Errorf("expected status=completed, got %v", parsed["status"])
		}
	})

	t.Run("falls back to latest response when no response.completed event", func(t *testing.T) {
		sseBody := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\"}}\n\ndata: [DONE]\n\n"
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseBody)),
		}
		synth, err := collectSSEToJSON(resp)
		if err != nil {
			t.Fatalf("expected fallback to latest response, got error: %v", err)
		}
		body, _ := io.ReadAll(synth.Body)
		if !strings.Contains(string(body), "resp_123") {
			t.Fatalf("expected fallback response to contain resp_123, got: %s", body)
		}
	})

	t.Run("reconstructs message output from SSE deltas when completed output is empty", func(t *testing.T) {
		sseBody := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_delta","status":"in_progress"}}`,
			"",
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress"}}`,
			"",
			`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text"}}`,
			"",
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello "}`,
			"",
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"world"}`,
			"",
			`data: {"type":"response.output_text.done","output_index":0,"content_index":0}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_delta","status":"completed","output":[]}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseBody)),
		}

		synth, err := collectSSEToJSON(resp)
		if err != nil {
			t.Fatalf("collectSSEToJSON() error = %v", err)
		}
		body, _ := io.ReadAll(synth.Body)
		var parsed struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("response body is not valid JSON: %s", string(body))
		}
		if len(parsed.Output) != 1 || parsed.Output[0].Type != "message" {
			t.Fatalf("output = %+v, want one message", parsed.Output)
		}
		if len(parsed.Output[0].Content) != 1 || parsed.Output[0].Content[0].Text != "hello world" {
			t.Fatalf("message content = %+v, want hello world", parsed.Output[0].Content)
		}
	})

	t.Run("reconstructs function call output from SSE deltas when completed output is empty", func(t *testing.T) {
		sseBody := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_call","status":"in_progress"}}`,
			"",
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup"}}`,
			"",
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\""}`,
			"",
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":":\"status\"}"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_call","status":"completed","output":[]}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseBody)),
		}

		synth, err := collectSSEToJSON(resp)
		if err != nil {
			t.Fatalf("collectSSEToJSON() error = %v", err)
		}
		body, _ := io.ReadAll(synth.Body)
		var parsed struct {
			Output []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"output"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("response body is not valid JSON: %s", string(body))
		}
		if len(parsed.Output) != 1 || parsed.Output[0].Type != "function_call" {
			t.Fatalf("output = %+v, want one function_call", parsed.Output)
		}
		if parsed.Output[0].CallID != "call_1" || parsed.Output[0].Name != "lookup" || parsed.Output[0].Arguments != `{"q":"status"}` {
			t.Fatalf("function call = %+v, want lookup call with arguments", parsed.Output[0])
		}
	})

	t.Run("error when no response data in any event", func(t *testing.T) {
		sseBody := "data: {\"type\":\"response.in_progress\"}\n\ndata: [DONE]\n\n"
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseBody)),
		}
		_, err := collectSSEToJSON(resp)
		if err == nil {
			t.Fatal("expected error when no response data in any event")
		}
	})

	t.Run("maybeCollectSSE passes through when not forced", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123"}`)),
		}
		result, err := maybeCollectSSE(resp, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != resp {
			t.Error("expected same response object when not forced")
		}
	})
}
