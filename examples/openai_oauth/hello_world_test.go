package openai_oauth

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestHelloWorldWithOpenAIOAuth(t *testing.T) {
	if liveTestsSkipped() {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	authJSONPath := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_AUTH_JSON_PATH"))
	if authJSONPath == "" {
		t.Skip("set OPENAI_OAUTH_AUTH_JSON_PATH to run this OpenAI OAuth example")
	}

	session, err := oauthSession(authJSONPath)
	if err != nil {
		t.Fatal(err)
	}

	model := envOr("OPENAI_OAUTH_MODEL", sdkopenai.DefaultChatModel)
	provider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		BaseURL:     envOr("OPENAI_BASE_URL", "https://chatgpt.com/backend-api/codex"),
		AuthMode:    sdkopenai.AuthModeOAuth,
		AuthSession: session,
	})
	runner := agentsdk.NewRunnerWithProvider(provider)

	result, err := runner.Run(context.Background(), helloAgent(model), helloInput(), helloConfig())
	if err != nil {
		t.Fatal(err)
	}

	requireHelloWorld(t, result.FinalText())
}

func oauthSession(authJSONPath string) (*sdkopenai.AuthSession, error) {
	return sdkopenai.NewOAuthAuthSessionFromConfig(sdkopenai.OAuthSessionConfig{
		AuthJSONPath:  authJSONPath,
		AccountID:     strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID")),
		AccountIDPath: strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID_PATH")),
	})
}

func helloAgent(model string) *agentsdk.Agent {
	return &agentsdk.Agent{
		Name:         "hello-openai-oauth",
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
		ModelSettings: agentsdk.ModelSettings{MaxTokens: 16, ReasoningEffort: "low"},
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
