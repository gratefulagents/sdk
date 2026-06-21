package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshOpenAIAuthJSON(t *testing.T) {
	now := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	cfg := RefreshConfig{
		Now:                 func() time.Time { return now },
		OpenAITokenEndpoint: "https://auth.example.test/oauth/token",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", req.Method)
			}
			if got := req.URL.String(); got != "https://auth.example.test/oauth/token" {
				t.Fatalf("url = %s, want custom endpoint", got)
			}
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" {
				t.Fatalf("request body = %#v", body)
			}
			return jsonResponse(http.StatusOK, `{"access_token":"new-access","refresh_token":"new-refresh"}`), nil
		})},
	}
	oldRefresh := now.Add(-OpenAIRefreshMaxAge - time.Hour).UTC().Format(time.RFC3339Nano)
	raw := []byte(`{"tokens":{"access_token":"old-access","refresh_token":"old-refresh","account_id":"acct"},"last_refresh":"` + oldRefresh + `"}`)

	updated, changed, err := RefreshOpenAIAuthJSON(context.Background(), raw, "", cfg)
	if err != nil {
		t.Fatalf("RefreshOpenAIAuthJSON() error = %v", err)
	}
	if !changed {
		t.Fatal("RefreshOpenAIAuthJSON() changed = false, want true")
	}
	if !strings.Contains(string(updated), "new-access") || !strings.Contains(string(updated), "new-refresh") {
		t.Fatalf("updated auth json = %s", updated)
	}
}

func TestRefreshAnthropicTokens(t *testing.T) {
	now := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	cfg := RefreshConfig{
		Now: func() time.Time { return now },
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", req.Method)
			}
			if got := req.URL.String(); got != AnthropicOAuthTokenURL {
				t.Fatalf("url = %s, want %s", got, AnthropicOAuthTokenURL)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if body["client_id"] != AnthropicOAuthClientID {
				t.Fatalf("client_id = %q, want %q", body["client_id"], AnthropicOAuthClientID)
			}
			if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" {
				t.Fatalf("refresh request body = %#v", body)
			}
			if body["scope"] != AnthropicOAuthScope {
				t.Fatalf("scope = %q, want %q", body["scope"], AnthropicOAuthScope)
			}
			return jsonResponse(http.StatusOK, `{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"expires_in":3600,
				"account":{"uuid":"acct-new","email_address":"user@example.com"}
			}`), nil
		})},
	}

	updated, err := RefreshAnthropicTokens(context.Background(), AnthropicAuth{
		RefreshToken: "old-refresh",
	}, cfg)
	if err != nil {
		t.Fatalf("RefreshAnthropicTokens() error = %v", err)
	}
	auth, err := ParseAnthropicAuthJSON(updated)
	if err != nil {
		t.Fatalf("ParseAnthropicAuthJSON() error = %v", err)
	}
	if auth.AccessToken != "new-access" || auth.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %#v", auth)
	}
	if auth.AccountUUID != "acct-new" || auth.Email != "user@example.com" {
		t.Fatalf("account = %#v", auth)
	}
	if !auth.LastRefresh.Equal(now) {
		t.Fatalf("LastRefresh = %v, want %v", auth.LastRefresh, now)
	}
	if want := now.Add(time.Hour); !auth.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", auth.ExpiresAt, want)
	}
}

func TestRefreshAnthropicTokensRedactsErrorBody(t *testing.T) {
	cfg := RefreshConfig{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusUnauthorized, `{
				"error":"refresh_token_reused",
				"access_token":"leaked-access",
				"refresh_token":"leaked-refresh"
			}`), nil
		})},
	}
	_, err := RefreshAnthropicTokens(context.Background(), AnthropicAuth{RefreshToken: "old-refresh"}, cfg)
	if err == nil {
		t.Fatal("RefreshAnthropicTokens() error = nil, want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "leaked-access") || strings.Contains(msg, "leaked-refresh") {
		t.Fatalf("error leaked token material: %s", msg)
	}
	if !IsTerminalRefreshError(err) {
		t.Fatalf("IsTerminalRefreshError(%v) = false, want true", err)
	}
}

func TestRefreshCopilotToken(t *testing.T) {
	now := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour).Unix()
	cfg := RefreshConfig{
		Now: func() time.Time { return now },
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("method = %s, want GET", req.Method)
			}
			if got := req.URL.String(); got != CopilotTokenURL {
				t.Fatalf("url = %s, want %s", got, CopilotTokenURL)
			}
			if got := req.Header.Get("Authorization"); got != "token github-oauth" {
				t.Fatalf("Authorization = %q, want token github-oauth", got)
			}
			if got := req.Header.Get("Accept"); got != "application/json" {
				t.Fatalf("Accept = %q, want application/json", got)
			}
			if got := req.Header.Get("Editor-Plugin-Version"); got == "" {
				t.Fatal("Editor-Plugin-Version header is empty")
			}
			return jsonResponse(http.StatusOK, `{"token":"copilot-api-token","expires_at":`+strconv.FormatInt(expiresAt, 10)+`}`), nil
		})},
	}

	updated, err := RefreshCopilotToken(context.Background(), CopilotAuth{
		OAuthToken: "github-oauth",
	}, cfg)
	if err != nil {
		t.Fatalf("RefreshCopilotToken() error = %v", err)
	}
	auth, err := ParseCopilotAuthJSON(updated)
	if err != nil {
		t.Fatalf("ParseCopilotAuthJSON() error = %v", err)
	}
	if auth.OAuthToken != "github-oauth" || auth.Token != "copilot-api-token" {
		t.Fatalf("tokens = %#v", auth)
	}
	if !auth.LastRefresh.Equal(now) {
		t.Fatalf("LastRefresh = %v, want %v", auth.LastRefresh, now)
	}
	if got := auth.ExpiresAt.Unix(); got != expiresAt {
		t.Fatalf("ExpiresAt = %d, want %d", got, expiresAt)
	}
}

func TestRefreshCopilotTokenRedactsErrorBody(t *testing.T) {
	cfg := RefreshConfig{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusUnauthorized, `{"token":"leaked-token","message":"bad credentials"}`), nil
		})},
	}
	_, err := RefreshCopilotToken(context.Background(), CopilotAuth{OAuthToken: "github-oauth"}, cfg)
	if err == nil {
		t.Fatal("RefreshCopilotToken() error = nil, want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "leaked-token") {
		t.Fatalf("error leaked token material: %s", msg)
	}
	if !IsTerminalRefreshError(err) {
		t.Fatalf("IsTerminalRefreshError(%v) = false, want true", err)
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
