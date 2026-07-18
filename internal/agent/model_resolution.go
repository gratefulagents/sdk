package agent

import (
	"strings"
)

const (
	defaultAnthropicModel = "claude-sonnet-4-6"
)

// ResolveModelForProvider maps short aliases (small/medium/large) and the
// empty model name to concrete defaults for the selected provider. Other
// names pass through trimmed; providers reject incompatible model IDs.
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
	trimmed := strings.TrimSpace(model)
	switch strings.ToLower(trimmed) {
	case "":
		return defaultAnthropicModel
	case "medium":
		return "claude-sonnet-4-6"
	case "large":
		return "claude-opus-4-6"
	case "small":
		return "claude-haiku-4-5"
	default:
		return trimmed
	}
}

func resolveOpenAIModel(model string) string {
	trimmed := strings.TrimSpace(model)
	switch strings.ToLower(trimmed) {
	case "":
		return ""
	case "medium":
		return "gpt-5.6-terra"
	case "large":
		return "gpt-5.6-sol"
	case "small":
		return "gpt-5.6-luna"
	default:
		return trimmed
	}
}
