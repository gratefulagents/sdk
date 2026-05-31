// Package liveopenai provides a shared OpenAI OAuth-backed runner for
// feature-example tests. Every non-skipped example test exercises real OpenAI
// via OAuth. Tests skip when credentials are missing unless
// GRATEFUL_LIVE_TESTS=required is set. Determinism is provided by loose,
// semantic assertions rather than deterministic mocks.
//
// Configuration:
//
//	OPENAI_OAUTH_AUTH_JSON_PATH   path to Codex auth.json (defaults to
//	                              $HOME/.codex/auth.json when present)
//	OPENAI_OAUTH_ACCOUNT_ID       optional account ID to bind
//	OPENAI_OAUTH_ACCOUNT_ID_PATH  optional path containing the account ID
//	OPENAI_BASE_URL               override Codex backend base URL
//	OPENAI_LIVE_MODEL             model name (defaults to gpt-5.5)
//	GRATEFUL_LIVE_TESTS=skip      skip live tests without touching provider
//	                              credentials or network
//	GRATEFUL_LIVE_TESTS=required  fail when required credentials are missing
package liveopenai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/livetest"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

// DefaultModel is the model name used when OPENAI_LIVE_MODEL is unset.
const DefaultModel = "gpt-5.5"

// Runner returns a Runner backed by the OpenAI OAuth provider plus the model
// name to use. Tests should pass the model name into Agent.Model and
// RunConfig.Model. Skips immediately when GRATEFUL_LIVE_TESTS=skip is set.
func Runner(t *testing.T) (*agentsdk.Runner, string) {
	t.Helper()
	provider, model := newProviderAndModel(t)
	return agentsdk.NewRunnerWithProvider(provider), model
}

// Provider returns just the configured live OpenAI provider, for tests that
// need to call provider.GetModel(...) directly (e.g. low-level streaming).
func Provider(t *testing.T) agentsdk.ModelProvider {
	t.Helper()
	provider, _ := newProviderAndModel(t)
	return provider
}

func newProviderAndModel(t *testing.T) (agentsdk.ModelProvider, string) {
	t.Helper()
	if livetest.Skipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	authPath := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_AUTH_JSON_PATH"))
	if authPath == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidate := filepath.Join(home, ".codex", "auth.json")
			if _, err := os.Stat(candidate); err == nil {
				authPath = candidate
			}
		}
	}
	if authPath == "" {
		livetest.MissingCredential(t, "set OPENAI_OAUTH_AUTH_JSON_PATH or install $HOME/.codex/auth.json to run live OpenAI OAuth tests")
	}
	if _, err := os.Stat(authPath); err != nil {
		t.Fatalf("OAuth auth JSON not available at %s: %v", authPath, err)
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	var session *sdkopenai.AuthSession
	if accountID := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID")); accountID != "" {
		authJSON, err := os.ReadFile(authPath)
		if err != nil {
			t.Fatal(err)
		}
		s, err := sdkopenai.NewOAuthAuthSessionFromSecretData(authJSON, accountID)
		if err != nil {
			t.Fatal(err)
		}
		session = s
	} else {
		s, err := sdkopenai.NewOAuthAuthSessionFromFile(authPath, strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID_PATH")))
		if err != nil {
			t.Fatal(err)
		}
		session = s
	}

	provider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		BaseURL:     baseURL,
		AuthMode:    sdkopenai.AuthModeOAuth,
		AuthSession: session,
	})

	model := strings.TrimSpace(os.Getenv("OPENAI_LIVE_MODEL"))
	if model == "" {
		model = DefaultModel
	}
	return provider, model
}
