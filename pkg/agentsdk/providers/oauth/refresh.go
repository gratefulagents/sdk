package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

// RefreshConfig configures provider OAuth refresh requests.
type RefreshConfig struct {
	HTTPClient *http.Client
	Now        func() time.Time

	OpenAIClientID      string
	OpenAITokenEndpoint string

	AnthropicClientID string
	AnthropicTokenURL string
	AnthropicScope    string

	CopilotTokenURL            string
	CopilotEditorVersion       string
	CopilotEditorPluginVersion string
	CopilotUserAgent           string
	CopilotAuthorizationScheme string
	ResponseBodyByteLimit      int64
}

// RefreshOpenAIAuthJSON refreshes serialized OpenAI OAuth material when it is
// stale and returns the resulting auth JSON plus whether it changed.
func RefreshOpenAIAuthJSON(ctx context.Context, authJSON []byte, accountID string, cfg RefreshConfig) ([]byte, bool, error) {
	now := refreshNow(cfg)
	needsRefresh, err := OpenAINeedsRefresh(authJSON, now)
	if err != nil {
		return nil, false, err
	}
	if !needsRefresh {
		return authJSON, false, nil
	}

	session, err := sdkopenai.NewOAuthAuthSessionFromConfig(sdkopenai.OAuthSessionConfig{
		AuthJSON:      authJSON,
		AccountID:     strings.TrimSpace(accountID),
		ClientID:      strings.TrimSpace(cfg.OpenAIClientID),
		TokenEndpoint: strings.TrimSpace(cfg.OpenAITokenEndpoint),
		RefreshClient: refreshHTTPClient(cfg),
	})
	if err != nil {
		return nil, false, fmt.Errorf("parse openai auth material: %w", err)
	}
	updated, err := session.RefreshAndSerialize(ctx, accountID)
	if err != nil {
		return nil, false, fmt.Errorf("refresh openai oauth material: %w", err)
	}
	return updated, true, nil
}

// RefreshAnthropicAuthJSON refreshes serialized Anthropic OAuth material when
// it is near expiry and returns the resulting auth JSON plus whether it changed.
func RefreshAnthropicAuthJSON(ctx context.Context, authJSON []byte, cfg RefreshConfig) ([]byte, bool, error) {
	auth, err := ParseAnthropicAuthJSON(authJSON)
	if err != nil {
		return nil, false, err
	}
	if !AnthropicNeedsRefresh(auth, refreshNow(cfg)) {
		return authJSON, false, nil
	}
	updated, err := RefreshAnthropicTokens(ctx, auth, cfg)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// RefreshAnthropicTokens exchanges an Anthropic OAuth refresh token and returns
// serialized flat auth JSON containing the updated access token.
func RefreshAnthropicTokens(ctx context.Context, auth AnthropicAuth, cfg RefreshConfig) ([]byte, error) {
	clientID := firstNonEmpty(cfg.AnthropicClientID, AnthropicOAuthClientID)
	tokenURL := firstNonEmpty(cfg.AnthropicTokenURL, AnthropicOAuthTokenURL)
	scope := firstNonEmpty(cfg.AnthropicScope, AnthropicOAuthScope)
	body, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"grant_type":    "refresh_token",
		"refresh_token": strings.TrimSpace(auth.RefreshToken),
		"scope":         scope,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build anthropic refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := refreshHTTPClient(cfg).Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic refresh request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseBodyByteLimit(cfg)))
	if err != nil {
		return nil, fmt.Errorf("read anthropic refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic refresh failed with status %d: %s", resp.StatusCode, SanitizeLogBody(string(raw)))
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Account      struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse anthropic refresh response: %w", err)
	}
	tokenResp.AccessToken = strings.TrimSpace(tokenResp.AccessToken)
	tokenResp.RefreshToken = strings.TrimSpace(tokenResp.RefreshToken)
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("anthropic refresh response missing access_token")
	}
	auth.AccessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		auth.RefreshToken = tokenResp.RefreshToken
	}
	now := refreshNow(cfg)
	if tokenResp.ExpiresIn > 0 {
		auth.ExpiresAt = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}
	if email := strings.TrimSpace(tokenResp.Account.EmailAddress); email != "" {
		auth.Email = email
	}
	if uuid := strings.TrimSpace(tokenResp.Account.UUID); uuid != "" {
		auth.AccountUUID = uuid
	}
	auth.LastRefresh = now
	return MarshalAnthropicAuthJSON(auth)
}

// RefreshCopilotAuthJSON refreshes serialized Copilot OAuth material when the
// Copilot API token is missing or near expiry.
func RefreshCopilotAuthJSON(ctx context.Context, authJSON []byte, cfg RefreshConfig) ([]byte, bool, error) {
	auth, err := ParseCopilotAuthJSON(authJSON)
	if err != nil {
		return nil, false, err
	}
	if !CopilotNeedsRefresh(auth, refreshNow(cfg)) {
		return authJSON, false, nil
	}
	updated, err := RefreshCopilotToken(ctx, auth, cfg)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// RefreshCopilotToken exchanges a GitHub OAuth token for a Copilot API token
// and returns serialized flat auth JSON.
func RefreshCopilotToken(ctx context.Context, auth CopilotAuth, cfg RefreshConfig) ([]byte, error) {
	oauthToken := strings.TrimSpace(auth.OAuthToken)
	if oauthToken == "" {
		return nil, fmt.Errorf("copilot oauth token is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, firstNonEmpty(cfg.CopilotTokenURL, CopilotTokenURL), nil)
	if err != nil {
		return nil, fmt.Errorf("build copilot token request: %w", err)
	}
	authScheme := firstNonEmpty(cfg.CopilotAuthorizationScheme, "token")
	req.Header.Set("Authorization", authScheme+" "+oauthToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", firstNonEmpty(cfg.CopilotEditorVersion, "gratefulagents-sdk/unknown"))
	req.Header.Set("Editor-Plugin-Version", firstNonEmpty(cfg.CopilotEditorPluginVersion, "gratefulagents-sdk/unknown"))
	req.Header.Set("User-Agent", firstNonEmpty(cfg.CopilotUserAgent, "gratefulagents-sdk"))

	resp, err := refreshHTTPClient(cfg).Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseBodyByteLimit(cfg)))
	if err != nil {
		return nil, fmt.Errorf("read copilot token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("copilot token request failed with status %d: %s", resp.StatusCode, SanitizeLogBody(string(raw)))
	}
	var tokenResp map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("parse copilot token response: %w", err)
	}
	token := strings.TrimSpace(stringValue(tokenResp, "token"))
	if token == "" {
		return nil, fmt.Errorf("copilot token response missing token")
	}
	auth.Token = token
	auth.ExpiresAt = firstTime(
		valueOf(tokenResp, "expires_at"),
		valueOf(tokenResp, "expiresAt"),
		valueOf(tokenResp, "expires"),
	)
	auth.LastRefresh = refreshNow(cfg)
	return MarshalCopilotAuthJSON(auth)
}

// IsTerminalRefreshError reports whether an error likely means the stored
// refresh material has been consumed or invalidated and needs user re-login.
func IsTerminalRefreshError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "refresh_token_reused") ||
		strings.Contains(msg, "refresh_token_expired") ||
		strings.Contains(msg, "refresh_token_invalidated") ||
		strings.Contains(msg, "status 401")
}

// SanitizeLogBody redacts token-like and secret-like JSON fields before logging
// provider OAuth error bodies.
func SanitizeLogBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if sanitized, err := json.Marshal(sanitizeJSONValue(parsed)); err == nil {
			body = string(sanitized)
		}
	}
	if len(body) > 2048 {
		return body[:2048] + "..."
	}
	return body
}

func sanitizeJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") || strings.Contains(lower, "secret") {
				out[key] = "[redacted]"
				continue
			}
			out[key] = sanitizeJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = sanitizeJSONValue(child)
		}
		return out
	default:
		return value
	}
}

func refreshHTTPClient(cfg RefreshConfig) *http.Client {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func refreshNow(cfg RefreshConfig) time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now()
}

func responseBodyByteLimit(cfg RefreshConfig) int64 {
	if cfg.ResponseBodyByteLimit > 0 {
		return cfg.ResponseBodyByteLimit
	}
	return 1 << 20
}
