package openrouter

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestHelloWorldWithOpenRouter(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		t.Skip("set OPENROUTER_API_KEY to run this OpenRouter example")
	}

	modelName := envOr("OPENROUTER_MODEL", "openai/gpt-4o-mini")
	client := sdkopenai.NewClient(
		apiKey,
		sdkopenai.WithBaseURL(envOr("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1")),
		sdkopenai.WithAPIMode("chat-completions"),
	)
	runner := agentsdk.NewRunnerWithModel(sdkopenai.NewModelWithClient(client))

	result, err := runner.Run(context.Background(), helloAgent(modelName), helloInput(), helloConfig())
	if err != nil {
		t.Fatal(err)
	}

	requireHelloWorld(t, result.FinalText())
}

func helloAgent(model string) *agentsdk.Agent {
	return &agentsdk.Agent{
		Name:         "hello-openrouter",
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
		ModelSettings: agentsdk.ModelSettings{MaxTokens: 16},
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
