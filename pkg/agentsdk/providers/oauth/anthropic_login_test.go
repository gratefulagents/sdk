package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGeneratePKCE(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() error = %v", err)
	}
	if pkce.Verifier == "" || pkce.Challenge == "" {
		t.Fatalf("GeneratePKCE() = %+v, want non-empty pair", pkce)
	}
	if _, err := base64.RawURLEncoding.DecodeString(pkce.Verifier); err != nil {
		t.Fatalf("verifier is not base64url: %v", err)
	}
	sum := sha256.Sum256([]byte(pkce.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); pkce.Challenge != want {
		t.Fatalf("challenge = %q, want S256(verifier) = %q", pkce.Challenge, want)
	}
	second, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() second error = %v", err)
	}
	if second.Verifier == pkce.Verifier {
		t.Fatal("GeneratePKCE() returned a repeated verifier")
	}
}

func TestNewAnthropicAuthorization(t *testing.T) {
	authz, err := NewAnthropicAuthorization("")
	if err != nil {
		t.Fatalf("NewAnthropicAuthorization() error = %v", err)
	}
	u, err := url.Parse(authz.URL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != AnthropicAuthorizeMaxURL {
		t.Fatalf("authorize base = %q, want %q (max is the default)", got, AnthropicAuthorizeMaxURL)
	}
	q := u.Query()
	if q.Get("client_id") != AnthropicOAuthClientID {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("code") != "true" || q.Get("response_type") != "code" {
		t.Fatalf("code/response_type = %q/%q", q.Get("code"), q.Get("response_type"))
	}
	if q.Get("redirect_uri") != AnthropicCodeCallbackURL || authz.RedirectURI != AnthropicCodeCallbackURL {
		t.Fatalf("redirect_uri = %q / %q", q.Get("redirect_uri"), authz.RedirectURI)
	}
	if q.Get("scope") != AnthropicOAuthScope {
		t.Fatalf("scope = %q, want %q", q.Get("scope"), AnthropicOAuthScope)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	sum := sha256.Sum256([]byte(authz.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); q.Get("code_challenge") != want {
		t.Fatalf("code_challenge = %q, want challenge of returned verifier", q.Get("code_challenge"))
	}
	if q.Get("state") == "" || q.Get("state") != authz.State {
		t.Fatalf("state = %q / %q", q.Get("state"), authz.State)
	}

	console, err := NewAnthropicAuthorization("console")
	if err != nil {
		t.Fatalf("NewAnthropicAuthorization(console) error = %v", err)
	}
	if !strings.HasPrefix(console.URL, AnthropicAuthorizeConsoleURL+"?") {
		t.Fatalf("console URL = %q, want %q base", console.URL, AnthropicAuthorizeConsoleURL)
	}

	if _, err := NewAnthropicAuthorization("bogus"); err == nil {
		t.Fatal("NewAnthropicAuthorization(bogus) error = nil, want error")
	}
}

func TestParseAnthropicCallbackInput(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		code    string
		state   string
		wantErr bool
	}{
		{name: "code hash state", input: " abc123#st456 ", code: "abc123", state: "st456"},
		{name: "full callback url", input: "https://platform.claude.com/oauth/code/callback?code=abc123&state=st456", code: "abc123", state: "st456"},
		{name: "query string", input: "code=abc123&state=st456", code: "abc123", state: "st456"},
		{name: "bare code", input: "abc123", code: "abc123", state: ""},
		{name: "url missing code", input: "https://platform.claude.com/oauth/code/callback?state=st456", wantErr: true},
		{name: "empty", input: "   ", wantErr: true},
		{name: "hash empty code", input: "#st456", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, state, err := ParseAnthropicCallbackInput(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAnthropicCallbackInput(%q) error = nil, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAnthropicCallbackInput(%q) error = %v", tc.input, err)
			}
			if code != tc.code || state != tc.state {
				t.Fatalf("ParseAnthropicCallbackInput(%q) = %q/%q, want %q/%q", tc.input, code, state, tc.code, tc.state)
			}
		})
	}
}

func TestExchangeAnthropicCode(t *testing.T) {
	now := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	authz := AnthropicAuthorization{
		RedirectURI: AnthropicCodeCallbackURL,
		State:       "expected-state",
		Verifier:    "the-verifier",
	}
	cfg := RefreshConfig{
		Now: func() time.Time { return now },
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", req.Method)
			}
			if got := req.URL.String(); got != AnthropicOAuthTokenURL {
				t.Fatalf("url = %s, want %s", got, AnthropicOAuthTokenURL)
			}
			if got := req.Header.Get("User-Agent"); got != anthropicTokenUserAgent {
				t.Fatalf("User-Agent = %q, want %q", got, anthropicTokenUserAgent)
			}
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			want := map[string]string{
				"grant_type":    "authorization_code",
				"code":          "auth-code",
				"state":         "expected-state",
				"client_id":     AnthropicOAuthClientID,
				"redirect_uri":  AnthropicCodeCallbackURL,
				"code_verifier": "the-verifier",
			}
			for k, v := range want {
				if body[k] != v {
					t.Fatalf("body[%q] = %q, want %q (body=%#v)", k, body[k], v, body)
				}
			}
			return jsonResponse(http.StatusOK, `{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"expires_in":3600,
				"account":{"uuid":"acct","email_address":"user@example.com"}
			}`), nil
		})},
	}

	raw, err := ExchangeAnthropicCode(context.Background(), "auth-code#expected-state", authz, cfg)
	if err != nil {
		t.Fatalf("ExchangeAnthropicCode() error = %v", err)
	}
	auth, err := ParseAnthropicAuthJSON(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicAuthJSON() error = %v", err)
	}
	if auth.AccessToken != "new-access" || auth.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %#v", auth)
	}
	if auth.Email != "user@example.com" || auth.AccountUUID != "acct" {
		t.Fatalf("account = %#v", auth)
	}
	if want := now.Add(time.Hour); !auth.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", auth.ExpiresAt, want)
	}
	if !auth.LastRefresh.Equal(now) {
		t.Fatalf("LastRefresh = %v, want %v", auth.LastRefresh, now)
	}
}

func TestExchangeAnthropicCodeStateMismatch(t *testing.T) {
	authz := AnthropicAuthorization{State: "expected", Verifier: "v"}
	cfg := RefreshConfig{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("token endpoint must not be called on state mismatch")
		return nil, nil
	})}}
	if _, err := ExchangeAnthropicCode(context.Background(), "code#other-state", authz, cfg); err == nil {
		t.Fatal("ExchangeAnthropicCode() error = nil, want state mismatch error")
	}
}

func TestExchangeAnthropicCodeRequiresVerifier(t *testing.T) {
	if _, err := ExchangeAnthropicCode(context.Background(), "code#state", AnthropicAuthorization{State: "state"}, RefreshConfig{}); err == nil {
		t.Fatal("ExchangeAnthropicCode() without verifier error = nil, want error")
	}
}

func TestExchangeAnthropicCodeRedactsErrorBody(t *testing.T) {
	authz := AnthropicAuthorization{State: "s", Verifier: "v"}
	cfg := RefreshConfig{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"error":"invalid_grant","access_token":"leaked"}`), nil
	})}}
	_, err := ExchangeAnthropicCode(context.Background(), "code#s", authz, cfg)
	if err == nil {
		t.Fatal("ExchangeAnthropicCode() error = nil, want error")
	}
	if strings.Contains(err.Error(), "leaked") {
		t.Fatalf("error leaked token material: %s", err)
	}
}
