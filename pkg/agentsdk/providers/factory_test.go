package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestNewProviderFromConfigRejectsUnknownProvider(t *testing.T) {
	if _, err := NewProviderFromConfig(ProviderSpec{Provider: "bogus"}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestNewProviderFromConfigSupportsLocal(t *testing.T) {
	provider, err := NewProviderFromConfig(ProviderSpec{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if provider == nil {
		t.Fatal("provider is nil")
	}
}

func TestNewProviderFromConfigConfiguresOpenAIOAuthForMulti(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	accountIDPath := filepath.Join(dir, "account-id")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}
	if err := os.WriteFile(accountIDPath, []byte("acct-from-path\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(account-id) error = %v", err)
	}

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:                 "multi",
		DefaultProvider:          "openai",
		Model:                    "openai/gpt-5.5",
		AuthMode:                 "oauth",
		OpenAIOAuthPath:          authPath,
		OpenAIOAuthAccountIDPath: accountIDPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("openai/gpt-5.5")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != "openai" {
		t.Fatalf("Provider() = %q, want openai", got)
	}
}

func TestNewProviderFromConfigMultiKeepsAnthropicFallbackAPIKeyWhenOpenAIOAuth(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	accountIDPath := filepath.Join(dir, "account-id")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}
	if err := os.WriteFile(accountIDPath, []byte("acct-from-path\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(account-id) error = %v", err)
	}

	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_test",
			"type":"message",
			"role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-sonnet-4-5",
			"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:                 "multi",
		DefaultProvider:          "openai",
		Model:                    "openai/gpt-5.5",
		AuthMode:                 "oauth",
		OpenAIOAuthPath:          authPath,
		OpenAIOAuthAccountIDPath: accountIDPath,
		ProviderAPIKeys:          map[string]string{"anthropic": "anthropic-api-key"},
		ProviderBaseURLs:         map[string]string{"anthropic": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model: "claude-sonnet-4-5",
		Input: []agentsdk.RunItem{{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "hello"},
		}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if gotAPIKey != "anthropic-api-key" {
		t.Fatalf("x-api-key = %q, want anthropic-api-key", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestNewProviderFromConfigMultiUsesAnthropicOAuthWhenDefaultProvider(t *testing.T) {
	var gotAuth, gotAPIKey, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_test",
			"type":"message",
			"role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-sonnet-4-5",
			"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:         "multi",
		DefaultProvider:  "anthropic",
		Model:            "anthropic/claude-sonnet-4-5",
		AuthMode:         "oauth",
		ProviderAPIKeys:  map[string]string{"anthropic": "anthropic-oauth-token"},
		ProviderBaseURLs: map[string]string{"anthropic": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model: "claude-sonnet-4-5",
		Input: []agentsdk.RunItem{{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "hello"},
		}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if gotAuth != "Bearer anthropic-oauth-token" {
		t.Fatalf("Authorization = %q, want Bearer anthropic-oauth-token", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("x-api-key = %q, want empty", gotAPIKey)
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Fatalf("anthropic-beta = %q, want oauth-2025-04-20", gotBeta)
	}
}

func TestNewProviderFromConfigMultiUsesConfiguredDefaultProvider(t *testing.T) {
	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:        "multi",
		DefaultProvider: "anthropic",
		ProviderAPIKeys: map[string]string{"anthropic": "sk-ant-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != "anthropic" {
		t.Fatalf("Provider() = %q, want anthropic", got)
	}
}
