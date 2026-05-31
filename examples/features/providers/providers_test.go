package providers_test

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestProviderConfigurationExample(t *testing.T) {
	openAIProvider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: "http://127.0.0.1:1/v1",
		APIMode: "chat-completions",
	})
	openAIModel, err := openAIProvider.GetModel("gpt-4.1-mini")
	if err != nil {
		t.Fatal(err)
	}
	if openAIModel.Provider() != "openai" {
		t.Fatalf("provider = %q", openAIModel.Provider())
	}

	anthropicProvider := sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: "http://127.0.0.1:1",
	})
	anthropicModel, err := anthropicProvider.GetModel("claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if anthropicModel.Provider() != "anthropic" {
		t.Fatalf("provider = %q", anthropicModel.Provider())
	}

	openRouterClient := sdkopenai.NewClient(
		"test-key",
		sdkopenai.WithBaseURL("https://openrouter.ai/api/v1"),
		sdkopenai.WithAPIMode("chat-completions"),
	)
	_ = agentsdk.NewRunnerWithModel(sdkopenai.NewModelWithClient(openRouterClient))

	if !sdkopenai.SupportsChatCompletions("gpt-4.1-mini") {
		t.Fatal("expected gpt-4.1-mini to support chat completions")
	}
	if err := sdkopenai.ValidateChatCompletionsModel(sdkopenai.DefaultChatMiniModel); err == nil {
		t.Fatalf("expected chat-completions validation to reject %q", sdkopenai.DefaultChatMiniModel)
	}
}
