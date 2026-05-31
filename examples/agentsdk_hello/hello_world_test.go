package agentsdk_hello

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestHelloWorldWithOpenAIProvider(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	authMode := strings.TrimSpace(os.Getenv("OPENAI_AUTH_MODE"))
	if apiKey == "" && !strings.EqualFold(authMode, string(sdkopenai.AuthModeOAuth)) {
		t.Skip("set OPENAI_API_KEY or OPENAI_AUTH_MODE=oauth to run this OpenAI integration example")
	}

	model := strings.TrimSpace(os.Getenv("OPENAI_HELLO_MODEL"))
	if model == "" {
		model = sdkopenai.DefaultChatModel
	}

	providerConfig := sdkopenai.ProviderConfig{
		BaseURL: strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		APIKey:  apiKey,
		APIMode: strings.TrimSpace(os.Getenv("OPENAI_API_MODE")),
	}
	if strings.EqualFold(authMode, string(sdkopenai.AuthModeOAuth)) {
		authPath := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_AUTH_JSON_PATH"))
		if authPath == "" {
			t.Skip("set OPENAI_OAUTH_AUTH_JSON_PATH to run this OpenAI OAuth integration example")
		}
		session, err := sdkopenai.NewOAuthAuthSessionFromConfig(sdkopenai.OAuthSessionConfig{
			AuthJSONPath:  authPath,
			AccountID:     strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID")),
			AccountIDPath: strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID_PATH")),
		})
		if err != nil {
			t.Fatal(err)
		}
		providerConfig.AuthMode = sdkopenai.AuthModeOAuth
		providerConfig.AuthSession = session
		if providerConfig.BaseURL == "" {
			providerConfig.BaseURL = "https://chatgpt.com/backend-api/codex"
		}
	}
	provider := sdkopenai.NewProviderWithConfig(providerConfig)
	runner := agentsdk.NewRunnerWithProvider(provider)

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "hello",
		Model:        model,
		Instructions: "Reply with exactly: hello world",
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Say hello world."},
		},
	}, agentsdk.RunConfig{
		MaxTurns:      1,
		ModelSettings: agentsdk.ModelSettings{MaxTokens: 16, ReasoningEffort: "low"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(strings.ToLower(result.FinalText())); got != "hello world" {
		t.Fatalf("FinalText() = %q, want %q", result.FinalText(), "hello world")
	}
}

func liveTestsSkipped() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_TESTS")), "skip")
}
