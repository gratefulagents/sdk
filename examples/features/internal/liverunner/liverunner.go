// Package liverunner is the central provider-dispatch helper used by every
// feature-example test. It returns a *agentsdk.Runner plus a model name,
// and chooses the underlying live provider based on GRATEFUL_LIVE_PROVIDER.
//
// Configuration:
//
//	GRATEFUL_LIVE_PROVIDER         which provider to back tests with.
//	                               Values: "openai" (default), "anthropic",
//	                               "openrouter", "multi". Aliases accepted:
//	                               "openai-oauth", "codex", "or".
//	GRATEFUL_LIVE_DEFAULT_PROVIDER default prefix for "multi" mode (one of
//	                               openai|anthropic|openrouter, default
//	                               "openai"). Other providers are
//	                               registered too when their credentials
//	                               are present, so handoffs/sub-agents can
//	                               name them via "<prefix>/<model>".
//	GRATEFUL_LIVE_TESTS=skip       skip live tests without touching provider
//	                               credentials or network.
//	GRATEFUL_LIVE_TESTS=required   fail when selected provider credentials
//	                               are missing.
//
// Per-provider credentials and overrides are documented in the liveopenai,
// liveanthropic, and liveopenrouter packages.
//
// In "multi" mode the returned model name is prefixed (e.g.
// "openai/gpt-5.5"), so it routes via agentsdk.MultiProvider's prefix
// parsing — the same MultiProvider construct the SDK exposes to users.
package liverunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liveanthropic"
	"github.com/gratefulagents/sdk/examples/features/internal/liveopenai"
	"github.com/gratefulagents/sdk/examples/features/internal/liveopenrouter"
	"github.com/gratefulagents/sdk/examples/features/internal/livetest"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// Runner returns a Runner backed by the live provider selected via
// GRATEFUL_LIVE_PROVIDER, plus the model name to use. The model name is
// already prefixed appropriately when running in "multi" mode.
func Runner(t *testing.T) (*agentsdk.Runner, string) {
	t.Helper()
	provider, model := provide(t)
	return agentsdk.NewRunnerWithProvider(provider), model
}

// Provider returns just the configured live provider, for tests that call
// provider.GetModel(...) directly.
func Provider(t *testing.T) agentsdk.ModelProvider {
	t.Helper()
	provider, _ := provide(t)
	return provider
}

// DefaultModel returns the model name selected for the current provider,
// honoring any *_LIVE_MODEL override, and prefixed in "multi" mode.
func DefaultModel(t *testing.T) string {
	t.Helper()
	_, model := provide(t)
	return model
}

func provide(t *testing.T) (agentsdk.ModelProvider, string) {
	t.Helper()
	if livetest.Skipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	switch resolveTarget() {
	case "anthropic":
		return liveanthropic.Provider(t), liveanthropic.DefaultModelName()
	case "openrouter":
		return liveopenrouter.Provider(t), liveopenrouter.DefaultModelName()
	case "multi":
		return provideMulti(t)
	default:
		return liveopenai.Provider(t), openaiModelName()
	}
}

func provideMulti(t *testing.T) (agentsdk.ModelProvider, string) {
	t.Helper()

	defaultPrefix := strings.ToLower(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_DEFAULT_PROVIDER")))
	if defaultPrefix == "" {
		defaultPrefix = "openai"
	}
	mp := agentsdk.NewMultiProvider(defaultPrefix)

	// Register every provider whose credentials are present. The default
	// provider is required; others are best-effort so multi mode still
	// produces a working runner when only one set of creds is configured.
	if hasOpenAICreds() {
		mp.Register("openai", liveopenai.Provider(t))
	} else if defaultPrefix == "openai" {
		failOrSkip(t, "GRATEFUL_LIVE_PROVIDER=multi with default=openai requires OpenAI OAuth credentials")
	}
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" {
		mp.Register("anthropic", liveanthropic.Provider(t))
	} else if defaultPrefix == "anthropic" {
		failOrSkip(t, "GRATEFUL_LIVE_PROVIDER=multi with default=anthropic requires ANTHROPIC_API_KEY")
	}
	if strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")) != "" {
		mp.Register("openrouter", liveopenrouter.Provider(t))
	} else if defaultPrefix == "openrouter" {
		failOrSkip(t, "GRATEFUL_LIVE_PROVIDER=multi with default=openrouter requires OPENROUTER_API_KEY")
	}

	model := defaultPrefix + "/" + defaultModelFor(defaultPrefix)
	return mp, model
}

func defaultModelFor(prefix string) string {
	switch prefix {
	case "anthropic":
		return liveanthropic.DefaultModelName()
	case "openrouter":
		return liveopenrouter.DefaultModelName()
	default:
		return openaiModelName()
	}
}

func openaiModelName() string {
	if m := strings.TrimSpace(os.Getenv("OPENAI_LIVE_MODEL")); m != "" {
		return m
	}
	return liveopenai.DefaultModel
}

func hasOpenAICreds() bool {
	if strings.TrimSpace(os.Getenv("OPENAI_OAUTH_AUTH_JSON_PATH")) != "" {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); err == nil {
			return true
		}
	}
	return false
}

func failOrSkip(t *testing.T, msg string) {
	t.Helper()
	livetest.MissingCredential(t, msg)
}

func resolveTarget() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_PROVIDER")))
	switch v {
	case "", "openai", "openai-oauth", "codex":
		return "openai"
	case "anthropic", "claude":
		return "anthropic"
	case "openrouter", "or":
		return "openrouter"
	case "multi", "multi-provider", "multiprovider":
		return "multi"
	default:
		return v
	}
}
