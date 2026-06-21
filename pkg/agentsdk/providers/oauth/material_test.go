package oauth

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func TestParseAnthropicAuthJSONAuth2APIShape(t *testing.T) {
	auth, err := ParseAnthropicAuthJSON([]byte(`{
		"access_token":"access",
		"refresh_token":"refresh",
		"last_refresh":"2026-06-01T00:00:00Z",
		"email":"user@example.com",
		"type":"claude",
		"expired":"2026-06-10T12:00:00Z",
		"account_uuid":"acct"
	}`))
	if err != nil {
		t.Fatalf("ParseAnthropicAuthJSON() error = %v", err)
	}
	if auth.AccessToken != "access" || auth.RefreshToken != "refresh" || auth.Email != "user@example.com" || auth.AccountUUID != "acct" {
		t.Fatalf("auth = %#v", auth)
	}
	if auth.ExpiresAt.IsZero() || auth.LastRefresh.IsZero() {
		t.Fatalf("expected parsed timestamps, got expires=%v last=%v", auth.ExpiresAt, auth.LastRefresh)
	}
}

func TestParseAnthropicAuthJSONClaudeCredentialsShape(t *testing.T) {
	expiresAt := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC).UnixMilli()
	raw, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "access",
			"refreshToken": "refresh",
			"expiresAt":    expiresAt,
			"tokenAccount": map[string]any{
				"uuid":         "acct",
				"emailAddress": "user@example.com",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := ParseAnthropicAuthJSON(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicAuthJSON() error = %v", err)
	}
	if auth.AccessToken != "access" || auth.RefreshToken != "refresh" || auth.Email != "user@example.com" || auth.AccountUUID != "acct" {
		t.Fatalf("auth = %#v", auth)
	}
	if got := auth.ExpiresAt.UnixMilli(); got != expiresAt {
		t.Fatalf("ExpiresAt = %d, want %d", got, expiresAt)
	}
}

func TestAnthropicNeedsRefresh(t *testing.T) {
	now := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	if !AnthropicNeedsRefresh(AnthropicAuth{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    now.Add(AnthropicRefreshLead),
	}, now) {
		t.Fatal("AnthropicNeedsRefresh at lead boundary = false, want true")
	}
	if AnthropicNeedsRefresh(AnthropicAuth{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    now.Add(AnthropicRefreshLead + time.Minute),
	}, now) {
		t.Fatal("AnthropicNeedsRefresh outside lead = true, want false")
	}
	if AnthropicNeedsRefresh(AnthropicAuth{AccessToken: "access"}, now) {
		t.Fatal("AnthropicNeedsRefresh without refresh token = true, want false")
	}
}

func TestParseCopilotAuthJSONFlatShape(t *testing.T) {
	expiresAt := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC).Unix()
	auth, err := ParseCopilotAuthJSON([]byte(`{
		"oauth_token":"github-oauth",
		"token":"copilot-api-token",
		"expires_at":` + strconv.FormatInt(expiresAt, 10) + `,
		"last_refresh":"2026-06-10T10:00:00Z",
		"type":"copilot"
	}`))
	if err != nil {
		t.Fatalf("ParseCopilotAuthJSON() error = %v", err)
	}
	if auth.OAuthToken != "github-oauth" || auth.Token != "copilot-api-token" {
		t.Fatalf("auth = %#v", auth)
	}
	if got := auth.ExpiresAt.Unix(); got != expiresAt {
		t.Fatalf("ExpiresAt = %d, want %d", got, expiresAt)
	}
	if auth.LastRefresh.IsZero() {
		t.Fatal("LastRefresh is zero")
	}
}

func TestParseCopilotAuthJSONGitHubHostShape(t *testing.T) {
	auth, err := ParseCopilotAuthJSON([]byte(`{"github.com":{"oauth_token":"github-oauth"}}`))
	if err != nil {
		t.Fatalf("ParseCopilotAuthJSON() error = %v", err)
	}
	if auth.OAuthToken != "github-oauth" {
		t.Fatalf("OAuthToken = %q, want github-oauth", auth.OAuthToken)
	}
}

func TestCopilotNeedsRefresh(t *testing.T) {
	now := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	if !CopilotNeedsRefresh(CopilotAuth{
		OAuthToken: "github-oauth",
		Token:      "copilot-api-token",
		ExpiresAt:  now.Add(CopilotRefreshLead),
	}, now) {
		t.Fatal("CopilotNeedsRefresh at lead boundary = false, want true")
	}
	if CopilotNeedsRefresh(CopilotAuth{
		OAuthToken: "github-oauth",
		Token:      "copilot-api-token",
		ExpiresAt:  now.Add(CopilotRefreshLead + time.Minute),
	}, now) {
		t.Fatal("CopilotNeedsRefresh outside lead = true, want false")
	}
	if !CopilotNeedsRefresh(CopilotAuth{OAuthToken: "github-oauth"}, now) {
		t.Fatal("CopilotNeedsRefresh without token = false, want true")
	}
	if CopilotNeedsRefresh(CopilotAuth{Token: "copilot-api-token"}, now) {
		t.Fatal("CopilotNeedsRefresh without oauth token = true, want false")
	}
}

func TestOpenAINeedsRefreshUsesLastRefreshAge(t *testing.T) {
	now := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	old := now.Add(-OpenAIRefreshMaxAge).UTC().Format(time.RFC3339Nano)
	authJSON := []byte(`{"tokens":{"access_token":"access","refresh_token":"refresh"},"last_refresh":"` + old + `"}`)
	needs, err := OpenAINeedsRefresh(authJSON, now)
	if err != nil {
		t.Fatalf("OpenAINeedsRefresh() error = %v", err)
	}
	if !needs {
		t.Fatal("OpenAINeedsRefresh at max age = false, want true")
	}

	fresh := now.Add(-OpenAIRefreshMaxAge + time.Minute).UTC().Format(time.RFC3339Nano)
	authJSON = []byte(`{"tokens":{"access_token":"access","refresh_token":"refresh"},"last_refresh":"` + fresh + `"}`)
	needs, err = OpenAINeedsRefresh(authJSON, now)
	if err != nil {
		t.Fatalf("OpenAINeedsRefresh() error = %v", err)
	}
	if needs {
		t.Fatal("OpenAINeedsRefresh before max age = true, want false")
	}
}
