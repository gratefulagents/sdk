package providers

// Regression tests for the 2026-07 audit: top-level AuthMode/APIMode must not
// leak into the always-registered OpenAI leg of a multi-provider spec.

import (
	"os"
	"path/filepath"
	"testing"

	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestAuthModeForOpenAIProviderScopesOAuthToOpenAI(t *testing.T) {
	cases := []struct {
		name string
		spec ProviderSpec
		want sdkopenai.AuthMode
	}{
		{
			name: "oauth for openai default stays oauth",
			spec: ProviderSpec{Provider: "multi", DefaultProvider: "openai", AuthMode: "oauth"},
			want: sdkopenai.AuthModeOAuth,
		},
		{
			name: "oauth for direct openai provider stays oauth",
			spec: ProviderSpec{Provider: "openai", AuthMode: "oauth"},
			want: sdkopenai.AuthModeOAuth,
		},
		{
			name: "oauth aimed at anthropic default does not leak",
			spec: ProviderSpec{Provider: "multi", DefaultProvider: "anthropic", AuthMode: "oauth"},
			want: sdkopenai.AuthModeAPIKey,
		},
		{
			name: "oauth with mounted openai oauth material stays oauth for copilot default",
			spec: ProviderSpec{Provider: "multi", DefaultProvider: "copilot", AuthMode: "oauth", OpenAIOAuthPath: "/var/run/oauth/openai/auth.json"},
			want: sdkopenai.AuthModeOAuth,
		},
		{
			name: "oauth with in-memory openai session stays oauth for anthropic default",
			spec: ProviderSpec{Provider: "multi", DefaultProvider: "anthropic", AuthMode: "oauth", OpenAIAuthSession: &sdkopenai.AuthSession{}},
			want: sdkopenai.AuthModeOAuth,
		},
		{
			name: "api-key mode with mounted openai oauth material stays api-key",
			spec: ProviderSpec{Provider: "multi", DefaultProvider: "copilot", AuthMode: "", OpenAIOAuthPath: "/var/run/oauth/openai/auth.json"},
			want: sdkopenai.AuthModeAPIKey,
		},
		{
			name: "api-key mode passes through",
			spec: ProviderSpec{Provider: "multi", DefaultProvider: "openai", AuthMode: ""},
			want: sdkopenai.AuthModeAPIKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authModeForOpenAIProvider(tc.spec); got != tc.want {
				t.Fatalf("authModeForOpenAIProvider(%+v) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}

func TestMultiProviderAnthropicOAuthDoesNotForceCodexBaseURLOnOpenAI(t *testing.T) {
	spec := ProviderSpec{
		Provider:        "multi",
		DefaultProvider: "anthropic",
		AuthMode:        "oauth",
		ProviderAPIKeys: map[string]string{"openai": "sk-test"},
	}
	provider, err := newOpenAIProviderFromSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if provider == nil {
		t.Fatal("nil provider")
	}
	// The regression put the OpenAI leg on the Codex OAuth backend; with the
	// scoping guard, auth mode is api-key so the base URL defaults to
	// api.openai.com. We can't reach into the model easily, so assert via the
	// scoped auth mode used by the builder.
	if got := authModeForOpenAIProvider(spec); got != sdkopenai.AuthModeAPIKey {
		t.Fatalf("openai leg auth mode = %q, want api-key", got)
	}
}

func TestAPIModeForCanonicalOpenAIIsScoped(t *testing.T) {
	// A top-level APIMode aimed at another default provider must not force the
	// canonical openai leg off its default.
	spec := ProviderSpec{
		Provider:        "multi",
		DefaultProvider: "openrouter",
		APIMode:         "chat-completions",
	}
	if got := apiModeForProvider(spec, DefaultProviderOpenAI, ""); got != "" {
		t.Fatalf("apiModeForProvider(openai) = %q, want \"\" (provider default)", got)
	}
	// Explicit per-provider setting still wins.
	spec.ProviderAPIModes = map[string]string{"openai": "responses"}
	if got := apiModeForProvider(spec, DefaultProviderOpenAI, ""); got != "responses" {
		t.Fatalf("apiModeForProvider(openai) with explicit map = %q, want responses", got)
	}
}

// TestMultiProviderResolvesOpenAIOAuthWhenDefaultIsCopilot reproduces the
// mid-run provider switch failure: a run started on copilot with OAuth (and
// the platform's additional OpenAI OAuth material mounted) must resolve
// "openai/..." models via OAuth instead of failing with "OpenAI API key is
// required".
func TestMultiProviderResolvesOpenAIOAuthWhenDefaultIsCopilot(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh","account_id":"acct-1"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(auth.json) error = %v", err)
	}

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:        "multi",
		DefaultProvider: "copilot",
		Model:           "copilot/claude-sonnet-4-5",
		AuthMode:        "oauth",
		OpenAIOAuthPath: authPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("openai/gpt-5.5")
	if err != nil {
		t.Fatalf("GetModel(openai/gpt-5.5) error = %v", err)
	}
	if got := model.Provider(); got != "openai" {
		t.Fatalf("Provider() = %q, want openai", got)
	}
}

// TestAuthModeForAnthropicProviderHonorsMountedOAuthPath mirrors the OpenAI
// case: explicit Anthropic OAuth material keeps the anthropic leg on OAuth
// even when the multi spec's default provider is another OAuth provider.
func TestAuthModeForAnthropicProviderHonorsMountedOAuthPath(t *testing.T) {
	spec := ProviderSpec{
		Provider:           "multi",
		DefaultProvider:    "copilot",
		AuthMode:           "oauth",
		AnthropicOAuthPath: "/var/run/oauth/anthropic/auth.json",
	}
	if got := authModeForAnthropicProvider(spec); got != "oauth" {
		t.Fatalf("authModeForAnthropicProvider() = %q, want oauth", got)
	}
	// Without mounted material the scoping guard still applies.
	spec.AnthropicOAuthPath = ""
	if got := authModeForAnthropicProvider(spec); got != "" {
		t.Fatalf("authModeForAnthropicProvider() without material = %q, want \"\"", got)
	}
	// The anthropic leg's OAuth config wires the file-backed token source.
	spec.AnthropicOAuthPath = "/var/run/oauth/anthropic/auth.json"
	cfg := anthropicProviderConfig(spec)
	if cfg.AuthMode != "oauth" || cfg.OAuthTokenSource == nil {
		t.Fatalf("anthropicProviderConfig() = {AuthMode:%q OAuthTokenSource:%v}, want oauth with token source", cfg.AuthMode, cfg.OAuthTokenSource)
	}
}
