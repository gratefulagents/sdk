package providers

import (
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

const (
	DefaultProviderOpenAI     = "openai"
	DefaultProviderAnthropic  = "anthropic"
	DefaultProviderOpenRouter = "openrouter"
	DefaultProviderGemini     = "gemini"
	DefaultProviderGroq       = "groq"
	DefaultProviderLocal      = "local"
	DefaultProviderCopilot    = "copilot"
)

// NewDefaultMultiProvider creates a MultiProvider with OpenAI and Anthropic
// registered and OpenAI as the default provider.
func NewDefaultMultiProvider() *agentsdk.MultiProvider {
	mp := agentsdk.NewMultiProvider(DefaultProviderOpenAI)
	mp.Register(DefaultProviderOpenAI, sdkopenai.NewProvider())
	mp.Register(DefaultProviderAnthropic, sdkanthropic.NewProvider())
	return mp
}

// NewRunner creates a Runner backed by the default provider set.
func NewRunner(model string) (*agentsdk.Runner, error) {
	resolved, err := NewDefaultMultiProvider().GetModel(model)
	if err != nil {
		return nil, err
	}
	return agentsdk.NewRunnerWithModel(resolved), nil
}
