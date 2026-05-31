package providers

import (
	"os"
	"path/filepath"
	"testing"
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
