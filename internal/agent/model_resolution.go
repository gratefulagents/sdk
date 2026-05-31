package agent

import (
	"strings"
)

const (
	defaultAnthropicModel = "claude-sonnet-4-6"
)

// ResolveModelForProvider maps short aliases and provider-incompatible model names
// to sensible defaults for the selected provider.
//
// Prefer using MultiProvider.GetModel() which handles this internally.
// This function is kept for call sites that need resolution without constructing a Model.
func ResolveModelForProvider(model, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "openai"
	}

	switch provider {
	case "openai":
		return resolveOpenAIModel(model)
	case "anthropic":
		return resolveAnthropicModel(model)
	default:
		return strings.TrimSpace(model)
	}
}

func resolveAnthropicModel(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "":
		return defaultAnthropicModel
	case "medium":
		return "claude-sonnet-4-6"
	case "large":
		return "claude-opus-4-6"
	case "small":
		return "claude-haiku-4-5"
	default:
		return model
	}
}

func resolveOpenAIModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	return trimmed
}
