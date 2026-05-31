// Package liveanthropic provides a shared Anthropic-backed runner for
// feature-example tests. Tests skip when ANTHROPIC_API_KEY is not set, unless
// GRATEFUL_LIVE_TESTS=required is exported.
//
// Configuration:
//
//	ANTHROPIC_API_KEY            enables live Anthropic tests
//	ANTHROPIC_BASE_URL           override Anthropic API base URL
//	ANTHROPIC_LIVE_MODEL         model name (defaults to claude-sonnet-4-6)
//	GRATEFUL_LIVE_TESTS=skip     skip live tests without touching provider
//	                             credentials or network
//	GRATEFUL_LIVE_TESTS=required fail when required credentials are missing
package liveanthropic

import (
	"os"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/livetest"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
)

// DefaultModel is the model name used when ANTHROPIC_LIVE_MODEL is unset.
const DefaultModel = "claude-sonnet-4-6"

// Runner returns a Runner backed by the live Anthropic provider plus the
// model name to use. Tests should pass the model name into Agent.Model.
func Runner(t *testing.T) (*agentsdk.Runner, string) {
	t.Helper()
	provider, model := newProviderAndModel(t)
	return agentsdk.NewRunnerWithProvider(provider), model
}

// Provider returns just the configured Anthropic provider, for tests that
// need to call provider.GetModel(...) directly.
func Provider(t *testing.T) agentsdk.ModelProvider {
	t.Helper()
	provider, _ := newProviderAndModel(t)
	return provider
}

// DefaultModelName returns the model name configured via env, or DefaultModel.
func DefaultModelName() string {
	if m := strings.TrimSpace(os.Getenv("ANTHROPIC_LIVE_MODEL")); m != "" {
		return m
	}
	return DefaultModel
}

func newProviderAndModel(t *testing.T) (agentsdk.ModelProvider, string) {
	t.Helper()
	if livetest.Skipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}

	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		livetest.MissingCredential(t, "set ANTHROPIC_API_KEY to run live Anthropic tests")
	}

	provider := sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
		APIKey:  apiKey,
		BaseURL: strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
	})

	return provider, DefaultModelName()
}
