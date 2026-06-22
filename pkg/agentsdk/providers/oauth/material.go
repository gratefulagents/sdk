package oauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	AuthJSONKey = "auth.json"

	OpenAIRefreshMaxAge    = 8 * 24 * time.Hour
	AnthropicRefreshLead   = 4 * time.Hour
	CopilotRefreshLead     = 5 * time.Minute
	AnthropicOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AnthropicOAuthTokenURL = "https://platform.claude.com/v1/oauth/token"
	AnthropicOAuthScope    = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	CopilotTokenURL        = "https://api.github.com/copilot_internal/v2/token"
)

type AnthropicAuth struct {
	AccessToken  string
	RefreshToken string
	Email        string
	AccountUUID  string
	ExpiresAt    time.Time
	LastRefresh  time.Time
}

type CopilotAuth struct {
	OAuthToken  string
	Token       string
	ExpiresAt   time.Time
	LastRefresh time.Time
}

// ParseAnthropicAuthJSON accepts both auth2api's flat token file shape and
// Claude Code's .credentials.json shape under claudeAiOauth.
func ParseAnthropicAuthJSON(data []byte) (AnthropicAuth, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return AnthropicAuth{}, fmt.Errorf("parse anthropic oauth json: %w", err)
	}
	claudeOAuth := mapValue(root, "claudeAiOauth")
	tokens := mapValue(root, "tokens")
	tokenAccount := mapValue(claudeOAuth, "tokenAccount")
	account := mapValue(root, "account")

	auth := AnthropicAuth{
		AccessToken: firstNonEmpty(
			stringValue(claudeOAuth, "accessToken"),
			stringValue(root, "access_token"),
			stringValue(root, "accessToken"),
			stringValue(tokens, "access_token"),
		),
		RefreshToken: firstNonEmpty(
			stringValue(claudeOAuth, "refreshToken"),
			stringValue(root, "refresh_token"),
			stringValue(root, "refreshToken"),
			stringValue(tokens, "refresh_token"),
		),
		Email: firstNonEmpty(
			stringValue(root, "email"),
			stringValue(tokenAccount, "emailAddress"),
			stringValue(account, "email_address"),
		),
		AccountUUID: firstNonEmpty(
			stringValue(root, "account_uuid"),
			stringValue(root, "accountUuid"),
			stringValue(tokenAccount, "uuid"),
			stringValue(account, "uuid"),
		),
		ExpiresAt: firstTime(
			valueOf(claudeOAuth, "expiresAt"),
			valueOf(root, "expired"),
			valueOf(root, "expires_at"),
			valueOf(root, "expiresAt"),
		),
		LastRefresh: firstTime(
			valueOf(root, "last_refresh"),
			valueOf(root, "lastRefresh"),
		),
	}
	if auth.AccessToken == "" && auth.RefreshToken == "" {
		return AnthropicAuth{}, fmt.Errorf("anthropic oauth material is missing both access and refresh tokens")
	}
	return auth, nil
}

func AnthropicNeedsRefresh(auth AnthropicAuth, now time.Time) bool {
	if strings.TrimSpace(auth.RefreshToken) == "" {
		return false
	}
	if strings.TrimSpace(auth.AccessToken) == "" {
		return true
	}
	if auth.ExpiresAt.IsZero() {
		return false
	}
	return !auth.ExpiresAt.After(now.Add(AnthropicRefreshLead))
}

func MarshalAnthropicAuthJSON(auth AnthropicAuth) ([]byte, error) {
	lastRefresh := auth.LastRefresh
	if lastRefresh.IsZero() {
		lastRefresh = time.Now()
	}
	out := map[string]any{
		"access_token":  strings.TrimSpace(auth.AccessToken),
		"refresh_token": strings.TrimSpace(auth.RefreshToken),
		"last_refresh":  lastRefresh.UTC().Format(time.RFC3339Nano),
		"type":          "claude",
	}
	if email := strings.TrimSpace(auth.Email); email != "" {
		out["email"] = email
	}
	if accountUUID := strings.TrimSpace(auth.AccountUUID); accountUUID != "" {
		out["account_uuid"] = accountUUID
	}
	if !auth.ExpiresAt.IsZero() {
		out["expired"] = auth.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return json.Marshal(out)
}

func AnthropicAccessToken(authJSON []byte) (string, error) {
	auth, err := ParseAnthropicAuthJSON(authJSON)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(auth.AccessToken)
	if token == "" {
		return "", fmt.Errorf("anthropic oauth material is missing access token")
	}
	return token, nil
}

// ParseCopilotAuthJSON accepts the SDK-managed flat shape as well as GitHub
// Copilot's host-keyed apps.json/hosts.json shape.
func ParseCopilotAuthJSON(data []byte) (CopilotAuth, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return CopilotAuth{}, fmt.Errorf("parse copilot oauth json: %w", err)
	}

	tokens := mapValue(root, "tokens")
	githubDotCom := copilotHostCredential(root)
	copilot := mapValue(root, "copilot")
	auth := CopilotAuth{
		OAuthToken: firstNonEmpty(
			stringValue(root, "oauth_token"),
			stringValue(root, "oauthToken"),
			stringValue(root, "github_oauth_token"),
			stringValue(tokens, "oauth_token"),
			stringValue(githubDotCom, "oauth_token"),
			stringValue(copilot, "oauth_token"),
		),
		Token: firstNonEmpty(
			stringValue(root, "token"),
			stringValue(root, "access_token"),
			stringValue(root, "accessToken"),
			stringValue(root, "copilot_token"),
			stringValue(tokens, "token"),
			stringValue(tokens, "access_token"),
			stringValue(copilot, "token"),
		),
		ExpiresAt: firstTime(
			valueOf(root, "expires_at"),
			valueOf(root, "expiresAt"),
			valueOf(root, "expires"),
			valueOf(tokens, "expires_at"),
			valueOf(copilot, "expires_at"),
		),
		LastRefresh: firstTime(
			valueOf(root, "last_refresh"),
			valueOf(root, "lastRefresh"),
			valueOf(copilot, "last_refresh"),
		),
	}
	if auth.OAuthToken == "" && auth.Token == "" {
		return CopilotAuth{}, fmt.Errorf("copilot oauth material is missing both oauth_token and token")
	}
	return auth, nil
}

func CopilotNeedsRefresh(auth CopilotAuth, now time.Time) bool {
	if strings.TrimSpace(auth.OAuthToken) == "" {
		return false
	}
	if strings.TrimSpace(auth.Token) == "" {
		return true
	}
	if auth.ExpiresAt.IsZero() {
		return false
	}
	return !auth.ExpiresAt.After(now.Add(CopilotRefreshLead))
}

func MarshalCopilotAuthJSON(auth CopilotAuth) ([]byte, error) {
	lastRefresh := auth.LastRefresh
	if lastRefresh.IsZero() {
		lastRefresh = time.Now()
	}
	out := map[string]any{
		"oauth_token":  strings.TrimSpace(auth.OAuthToken),
		"token":        strings.TrimSpace(auth.Token),
		"last_refresh": lastRefresh.UTC().Format(time.RFC3339Nano),
		"type":         "copilot",
	}
	if !auth.ExpiresAt.IsZero() {
		out["expires_at"] = auth.ExpiresAt.UTC().Unix()
	}
	return json.Marshal(out)
}

func CopilotAPIToken(authJSON []byte) (string, error) {
	auth, err := ParseCopilotAuthJSON(authJSON)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(auth.Token)
	if token == "" {
		return "", fmt.Errorf("copilot oauth material is missing Copilot API token")
	}
	return token, nil
}

func OpenAINeedsRefresh(authJSON []byte, now time.Time) (bool, error) {
	var parsed struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(authJSON, &parsed); err != nil {
		return false, fmt.Errorf("parse openai oauth json: %w", err)
	}
	if strings.TrimSpace(parsed.Tokens.RefreshToken) == "" {
		return false, nil
	}
	if strings.TrimSpace(parsed.Tokens.AccessToken) == "" {
		return true, nil
	}
	lastRefresh := parseTime(parsed.LastRefresh)
	if lastRefresh.IsZero() {
		return false, nil
	}
	return !lastRefresh.After(now.Add(-OpenAIRefreshMaxAge)), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func mapValue(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	child, _ := m[key].(map[string]any)
	return child
}

// copilotHostCredential locates the GitHub host credential entry within a
// Copilot apps.json/hosts.json document. It prefers an exact "github.com" key,
// then any "github.com:<client_id>" enterprise/app key, and finally any child
// object that carries an oauth_token or token.
func copilotHostCredential(root map[string]any) map[string]any {
	if exact := mapValue(root, "github.com"); exact != nil {
		return exact
	}
	for key, value := range root {
		if key != "github.com" && !strings.HasPrefix(key, "github.com:") {
			continue
		}
		child, _ := value.(map[string]any)
		if child != nil {
			return child
		}
	}
	for _, value := range root {
		child, _ := value.(map[string]any)
		if child == nil {
			continue
		}
		if stringValue(child, "oauth_token") != "" || stringValue(child, "token") != "" {
			return child
		}
	}
	return nil
}

func valueOf(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func stringValue(m map[string]any, key string) string {
	if value, ok := valueOf(m, key).(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func firstTime(values ...any) time.Time {
	for _, value := range values {
		if parsed := parseFlexibleTime(value); !parsed.IsZero() {
			return parsed
		}
	}
	return time.Time{}
}

func parseFlexibleTime(value any) time.Time {
	switch v := value.(type) {
	case string:
		return parseTime(v)
	case float64:
		return unixMillis(v)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return unixMillis(f)
		}
	case int64:
		return unixMillis(float64(v))
	case int:
		return unixMillis(float64(v))
	}
	return time.Time{}
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return unixMillis(float64(n))
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func unixMillis(value float64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	// Claude Code stores expiresAt as milliseconds. Accept seconds too for
	// hand-authored secrets.
	if value < 1e12 {
		return time.Unix(int64(value), 0)
	}
	return time.UnixMilli(int64(value))
}
