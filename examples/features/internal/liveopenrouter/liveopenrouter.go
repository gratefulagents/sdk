// Package liveopenrouter provides a shared OpenRouter-backed runner for
// feature-example tests. OpenRouter exposes an OpenAI-compatible API, so the
// SDK's openai provider is reused with a custom BaseURL and chat-completions
// API mode.
//
// Configuration:
//
//	OPENROUTER_API_KEY           enables live OpenRouter tests
//	OPENROUTER_BASE_URL          override OpenRouter base URL
//	                             (defaults to https://openrouter.ai/api/v1)
//	OPENROUTER_LIVE_MODEL        model name (defaults to deepseek/deepseek-v4-flash)
//	GRATEFUL_LIVE_TESTS=skip     skip live tests without touching provider
//	                             credentials or network
//	GRATEFUL_LIVE_TESTS=required fail when required credentials are missing
package liveopenrouter

import (
	"os"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/livetest"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

// DefaultModel is used when OPENROUTER_LIVE_MODEL is unset. Picked for low
// cost and broad availability on OpenRouter.
const DefaultModel = "deepseek/deepseek-v4-flash"

// DefaultBaseURL is OpenRouter's OpenAI-compatible endpoint.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// Runner returns a Runner backed by OpenRouter (via the openai-compatible
// provider) plus the model name to use.
func Runner(t *testing.T) (*agentsdk.Runner, string) {
	t.Helper()
	provider, model := newProviderAndModel(t)
	return agentsdk.NewRunnerWithProvider(provider), model
}

// Provider returns the configured OpenRouter provider for tests that call
// provider.GetModel(...) directly.
func Provider(t *testing.T) agentsdk.ModelProvider {
	t.Helper()
	provider, _ := newProviderAndModel(t)
	return provider
}

// DefaultModelName returns the model name configured via env, or DefaultModel.
func DefaultModelName() string {
	if m := strings.TrimSpace(os.Getenv("OPENROUTER_LIVE_MODEL")); m != "" {
		return m
	}
	return DefaultModel
}

func newProviderAndModel(t *testing.T) (agentsdk.ModelProvider, string) {
	t.Helper()
	if livetest.Skipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}

	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		livetest.MissingCredential(t, "set OPENROUTER_API_KEY to run live OpenRouter tests")
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL"))
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	provider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		APIKey:   apiKey,
		BaseURL:  baseURL,
		AuthMode: sdkopenai.AuthModeAPIKey,
		APIMode:  "chat-completions",
	})

	return provider, DefaultModelName()
}
