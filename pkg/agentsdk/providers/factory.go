package providers

import (
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

const DefaultCodexBackendBaseURL = "https://chatgpt.com/backend-api/codex"

type ProviderSpec struct {
	Provider                 string
	DefaultProvider          string
	Model                    string
	BaseURL                  string
	APIKey                   string
	AuthMode                 string
	APIMode                  string
	OpenAIOAuthPath          string
	OpenAIOAuthAccountID     string
	OpenAIOAuthAccountIDPath string
	OpenAIAuthSession        *sdkopenai.AuthSession
	ProviderAPIKeys          map[string]string
	ProviderBaseURLs         map[string]string
	ProviderAPIModes         map[string]string
	// ModelFallbacks is an ordered list of fallback model identifiers sent as
	// the OpenRouter "models" array so the provider retries the next model when
	// one is unavailable. Applies to OpenAI-compatible providers (openrouter,
	// gemini, groq, local, openai-api). Empty disables fallback routing.
	ModelFallbacks []string
}

func NewProviderFromConfig(spec ProviderSpec) (agentsdk.ModelProvider, error) {
	provider := strings.ToLower(strings.TrimSpace(spec.Provider))
	if provider == "" {
		provider = DefaultProviderOpenAI
	}
	switch provider {
	case "multi":
		return newMultiProviderFromSpec(spec)
	case "openai":
		return newOpenAIProviderFromSpec(spec)
	case "anthropic":
		return sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
			BaseURL: baseURLForProvider(spec, DefaultProviderAnthropic),
			APIKey:  apiKeyForProvider(spec, DefaultProviderAnthropic),
		}), nil
	case "openrouter":
		return newOpenAICompatibleProviderFromSpec("openrouter", spec), nil
	case "gemini":
		return newOpenAICompatibleProviderFromSpec("gemini", spec), nil
	case "groq":
		return newOpenAICompatibleProviderFromSpec("groq", spec), nil
	case "local":
		return newOpenAICompatibleProviderFromSpec("local", spec), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", spec.Provider)
	}
}

func NewRunnerFromConfig(spec ProviderSpec) (*agentsdk.Runner, error) {
	provider, err := NewProviderFromConfig(spec)
	if err != nil {
		return nil, err
	}
	return agentsdk.NewRunnerWithProvider(provider), nil
}

func newOpenAIProviderFromSpec(spec ProviderSpec) (agentsdk.ModelProvider, error) {
	baseURL := baseURLForProvider(spec, DefaultProviderOpenAI)
	authMode := sdkopenai.NormalizeAuthMode(spec.AuthMode)
	apiKey := apiKeyForProvider(spec, DefaultProviderOpenAI)
	session := spec.OpenAIAuthSession

	if authMode == sdkopenai.AuthModeOAuth {
		if baseURL == "" {
			baseURL = DefaultCodexBackendBaseURL
		}
		authPath := strings.TrimSpace(spec.OpenAIOAuthPath)
		if session == nil && authPath != "" {
			var err error
			session, err = sdkopenai.NewOAuthAuthSessionFromConfig(sdkopenai.OAuthSessionConfig{
				AuthJSONPath:  authPath,
				AccountID:     spec.OpenAIOAuthAccountID,
				AccountIDPath: strings.TrimSpace(spec.OpenAIOAuthAccountIDPath),
			})
			if err != nil {
				return nil, fmt.Errorf("load OpenAI OAuth session from %s: %w", authPath, err)
			}
		}
	} else {
		if baseURL == "" || baseURL == DefaultCodexBackendBaseURL {
			baseURL = "https://api.openai.com/v1"
		}
	}

	return sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		BaseURL:     baseURL,
		APIKey:      apiKey,
		AuthMode:    authMode,
		APIMode:     apiModeForProvider(spec, DefaultProviderOpenAI, spec.APIMode),
		AuthSession: session,
	}), nil
}

func newMultiProviderFromSpec(spec ProviderSpec) (agentsdk.ModelProvider, error) {
	defaultProvider := defaultProviderForSpec(spec)
	mp := agentsdk.NewMultiProvider(defaultProvider)

	openAIProvider, err := newOpenAIProviderFromSpec(spec)
	if err != nil {
		return nil, err
	}
	mp.Register(DefaultProviderOpenAI, openAIProvider)
	mp.Register(DefaultProviderAnthropic, sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
		BaseURL: baseURLForProvider(spec, DefaultProviderAnthropic),
		APIKey:  apiKeyForProvider(spec, DefaultProviderAnthropic),
	}))
	for _, provider := range []string{"openrouter", "gemini", "groq", "local"} {
		mp.Register(provider, newOpenAICompatibleProviderFromSpec(provider, spec))
	}
	return mp, nil
}

func newOpenAICompatibleProviderFromSpec(provider string, spec ProviderSpec) agentsdk.ModelProvider {
	provider = normalizeProviderName(provider)
	baseURL := firstNonEmpty(baseURLForProvider(spec, provider), defaultBaseURLForProvider(provider))
	apiKey := apiKeyForProvider(spec, provider)
	if provider == "local" {
		apiKey = firstNonEmpty(apiKey, "local-key")
	}
	// Model fallbacks are sent as the request-body "models" array, which is an
	// OpenRouter routing feature. Other OpenAI-compatible backends may reject an
	// unknown "models" field, so only forward fallbacks to OpenRouter.
	var modelFallbacks []string
	if provider == "openrouter" {
		modelFallbacks = spec.ModelFallbacks
	}
	return sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		BaseURL:        baseURL,
		APIKey:         apiKey,
		APIMode:        apiModeForProvider(spec, provider, "chat-completions"),
		AuthMode:       sdkopenai.AuthModeAPIKey,
		ModelFallbacks: modelFallbacks,
	})
}

func defaultProviderForSpec(spec ProviderSpec) string {
	if provider := normalizeProviderName(spec.DefaultProvider); provider != "" {
		return provider
	}
	if prefix, _ := agentsdk.ParseModelPrefix(spec.Model); strings.TrimSpace(prefix) != "" {
		return normalizeProviderName(prefix)
	}
	provider := normalizeProviderName(spec.Provider)
	if provider != "" && provider != "multi" {
		return provider
	}
	return DefaultProviderOpenAI
}

func apiKeyForProvider(spec ProviderSpec, provider string) string {
	provider = normalizeProviderName(provider)
	if provider == "" {
		return ""
	}
	if value := lookupProviderValue(spec.ProviderAPIKeys, provider); value != "" {
		return value
	}
	apiKey := strings.TrimSpace(spec.APIKey)
	if apiKey == "" {
		return ""
	}
	configuredProvider := normalizeProviderName(spec.Provider)
	if configuredProvider == "" || configuredProvider == provider {
		return apiKey
	}
	if configuredProvider == "multi" && provider == defaultProviderForSpec(spec) {
		return apiKey
	}
	return ""
}

func baseURLForProvider(spec ProviderSpec, provider string) string {
	provider = normalizeProviderName(provider)
	if provider == "" {
		return ""
	}
	if value := lookupProviderValue(spec.ProviderBaseURLs, provider); value != "" {
		return value
	}
	baseURL := strings.TrimSpace(spec.BaseURL)
	if baseURL == "" {
		return ""
	}
	configuredProvider := normalizeProviderName(spec.Provider)
	if configuredProvider == "" || configuredProvider == provider {
		return baseURL
	}
	if configuredProvider == "multi" && provider == defaultProviderForSpec(spec) {
		return baseURL
	}
	return ""
}

func apiModeForProvider(spec ProviderSpec, provider, fallback string) string {
	provider = normalizeProviderName(provider)
	if value := lookupProviderValue(spec.ProviderAPIModes, provider); value != "" {
		return value
	}
	if value := strings.TrimSpace(spec.APIMode); value != "" {
		configuredProvider := normalizeProviderName(spec.Provider)
		if configuredProvider == "" || configuredProvider == provider || (configuredProvider == "multi" && provider == defaultProviderForSpec(spec)) {
			return value
		}
	}
	return fallback
}

func lookupProviderValue(values map[string]string, provider string) string {
	for key, value := range values {
		if normalizeProviderName(key) == provider {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func defaultBaseURLForProvider(provider string) string {
	switch normalizeProviderName(provider) {
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai"
	case "groq":
		return "https://api.groq.com/openai/v1"
	case "local":
		return "http://localhost:11434/v1"
	default:
		return ""
	}
}

func normalizeProviderName(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
