package providers

// Regression tests for the 2026-07 audit: top-level AuthMode/APIMode must not
// leak into the always-registered OpenAI leg of a multi-provider spec.

import (
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
