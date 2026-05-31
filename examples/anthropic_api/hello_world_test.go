package anthropic_api

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
)

func TestHelloWorldWithAnthropicAPIKey(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("set ANTHROPIC_API_KEY to run this Anthropic API-key example")
	}

	model := envOr("ANTHROPIC_MODEL", "claude-haiku-4-5")
	provider := sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
		APIKey:  apiKey,
		BaseURL: strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
	})
	runner := agentsdk.NewRunnerWithProvider(provider)

	result, err := runner.Run(context.Background(), helloAgent(model), helloInput(), helloConfig())
	if err != nil {
		t.Fatal(err)
	}

	requireHelloWorld(t, result.FinalText())
}

func helloAgent(model string) *agentsdk.Agent {
	return &agentsdk.Agent{
		Name:         "hello-anthropic-api",
		Model:        model,
		Instructions: "Reply with exactly: hello world",
	}
}

func helloInput() []agentsdk.RunItem {
	return []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Say hello world."},
		},
	}
}

func helloConfig() agentsdk.RunConfig {
	return agentsdk.RunConfig{
		MaxTurns:      1,
		ModelSettings: agentsdk.ModelSettings{MaxTokens: 32},
	}
}

func requireHelloWorld(t *testing.T, text string) {
	t.Helper()

	if got := normalize(text); got != "hello world" {
		t.Fatalf("FinalText() = %q, want %q", text, "hello world")
	}
}

func normalize(text string) string {
	return strings.Trim(strings.TrimSpace(strings.ToLower(text)), ".!")
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func liveTestsSkipped() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_TESTS")), "skip")
}
