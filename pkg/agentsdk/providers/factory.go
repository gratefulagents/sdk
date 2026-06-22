package providers

import (
	"context"
	"fmt"
	"os"
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
	// one is unavailable. It is only forwarded to OpenRouter; other
	// OpenAI-compatible providers ignore it. Empty disables fallback routing.
	ModelFallbacks []string
	// Routes declares named provider instances registered under arbitrary
	// routing prefixes, on top of the canonical provider set. Routes let a
	// single MultiProvider expose the same base provider under multiple
	// prefixes with independent auth/credentials, so callers can route by model
	// prefix (e.g. "anthropic/..." vs "anthropic-oauth/...") to select API-key
	// vs OAuth per request. A non-empty Routes list implies multi-provider
	// behavior regardless of Provider.
	Routes []ProviderRoute
}

// ProviderRoute declares a single provider instance registered under an
// arbitrary routing prefix. Routes are registered after the canonical provider
// set, so a route whose Prefix matches a canonical provider name (e.g.
// "anthropic") overrides that default registration.
type ProviderRoute struct {
	// Prefix is the routing key matched against a model's "prefix/model"
	// segment. When empty it defaults to Provider.
	Prefix string
	// Provider is the base provider type to build (e.g. "openai", "anthropic",
	// "copilot", "openrouter"). Required; "multi" is not allowed.
	Provider string
	BaseURL  string
	APIKey   string
	// AuthMode selects the auth scheme for this route (e.g. "oauth" or
	// "api_key"). Empty uses the provider's default (API key). For OAuth, the
	// token is supplied via APIKey for anthropic/copilot, or via the
	// OpenAIOAuth* fields for openai.
	AuthMode string
	APIMode  string

	// OpenAI OAuth configuration, used when Provider is "openai" and AuthMode
	// is "oauth".
	OpenAIOAuthPath          string
	OpenAIOAuthAccountID     string
	OpenAIOAuthAccountIDPath string
	OpenAIAuthSession        *sdkopenai.AuthSession
}

var openAICompatibleProviderNames = []string{
	DefaultProviderOpenRouter,
	DefaultProviderGemini,
	DefaultProviderGroq,
	DefaultProviderLocal,
}

const defaultCopilotBaseURL = "https://api.githubcopilot.com"

func NewProviderFromConfig(spec ProviderSpec) (agentsdk.ModelProvider, error) {
	provider := strings.ToLower(strings.TrimSpace(spec.Provider))
	if provider == "" {
		provider = DefaultProviderOpenAI
	}
	// Named routes require a MultiProvider for prefix-based dispatch, so a
	// non-empty Routes list upgrades any single-provider config to multi while
	// preserving the configured provider as the default route.
	if len(spec.Routes) > 0 {
		provider = "multi"
	}
	switch provider {
	case "multi":
		return newMultiProviderFromSpec(spec)
	case DefaultProviderOpenAI:
		return newOpenAIProviderFromSpec(spec)
	case DefaultProviderAnthropic:
		return sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
			BaseURL:  baseURLForProvider(spec, DefaultProviderAnthropic),
			APIKey:   apiKeyForProvider(spec, DefaultProviderAnthropic),
			AuthMode: authModeForAnthropicProvider(spec),
		}), nil
	case DefaultProviderOpenRouter, DefaultProviderGemini, DefaultProviderGroq, DefaultProviderLocal:
		return newOpenAICompatibleProviderFromSpec(provider, spec), nil
	case DefaultProviderCopilot:
		return newCopilotProviderFromSpec(spec), nil
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
		BaseURL:  baseURLForProvider(spec, DefaultProviderAnthropic),
		APIKey:   apiKeyForProvider(spec, DefaultProviderAnthropic),
		AuthMode: authModeForAnthropicProvider(spec),
	}))
	for _, provider := range openAICompatibleProviderNames {
		mp.Register(provider, newOpenAICompatibleProviderFromSpec(provider, spec))
	}
	mp.Register(DefaultProviderCopilot, newCopilotProviderFromSpec(spec))
	if err := registerProviderRoutes(mp, spec.Routes); err != nil {
		return nil, err
	}
	return mp, nil
}

// registerProviderRoutes builds and registers each named route on the
// MultiProvider. Routes are applied after the canonical providers, so a route
// prefix matching a canonical name overrides that default registration.
func registerProviderRoutes(mp *agentsdk.MultiProvider, routes []ProviderRoute) error {
	for _, route := range routes {
		base := normalizeProviderName(route.Provider)
		if base == "" {
			return fmt.Errorf("provider route %q: Provider is required", route.Prefix)
		}
		if base == "multi" {
			return fmt.Errorf("provider route %q: Provider %q is not allowed", route.Prefix, route.Provider)
		}
		prefix := normalizeProviderName(route.Prefix)
		if prefix == "" {
			prefix = base
		}
		routeProvider, err := NewProviderFromConfig(specForRoute(route, base))
		if err != nil {
			return fmt.Errorf("provider route %q: %w", prefix, err)
		}
		mp.Register(prefix, routeProvider)
	}
	return nil
}

// specForRoute converts a ProviderRoute into a single-provider ProviderSpec so
// route construction reuses the canonical per-provider builders (including
// OAuth session loading and base-URL defaults).
func specForRoute(route ProviderRoute, base string) ProviderSpec {
	return ProviderSpec{
		Provider:                 base,
		BaseURL:                  route.BaseURL,
		APIKey:                   route.APIKey,
		AuthMode:                 route.AuthMode,
		APIMode:                  route.APIMode,
		OpenAIOAuthPath:          route.OpenAIOAuthPath,
		OpenAIOAuthAccountID:     route.OpenAIOAuthAccountID,
		OpenAIOAuthAccountIDPath: route.OpenAIOAuthAccountIDPath,
		OpenAIAuthSession:        route.OpenAIAuthSession,
	}
}

func newOpenAICompatibleProviderFromSpec(provider string, spec ProviderSpec) agentsdk.ModelProvider {
	provider = normalizeProviderName(provider)
	baseURL := firstNonEmpty(baseURLForProvider(spec, provider), defaultBaseURLForProvider(provider))
	apiKey := apiKeyForProvider(spec, provider)
	if fallbackKey := defaultAPIKeyForProvider(provider); fallbackKey != "" {
		apiKey = firstNonEmpty(apiKey, fallbackKey)
	}
	// Model fallbacks are sent as the request-body "models" array, which is an
	// OpenRouter routing feature. Other OpenAI-compatible backends may reject an
	// unknown "models" field, so only forward fallbacks to OpenRouter.
	var modelFallbacks []string
	if provider == DefaultProviderOpenRouter {
		modelFallbacks = spec.ModelFallbacks
	}
	return sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		ProviderName:   provider,
		BaseURL:        baseURL,
		APIKey:         apiKey,
		APIMode:        apiModeForProvider(spec, provider, "chat-completions"),
		AuthMode:       sdkopenai.AuthModeAPIKey,
		ModelFallbacks: modelFallbacks,
	})
}

func newCopilotProviderFromSpec(spec ProviderSpec) agentsdk.ModelProvider {
	apiKey := apiKeyForProvider(spec, DefaultProviderCopilot)
	baseURL := firstNonEmpty(baseURLForProvider(spec, DefaultProviderCopilot), defaultCopilotBaseURL)
	openAIHeaders := func(context.Context) (map[string]string, error) {
		token := strings.TrimSpace(apiKey)
		if token == "" {
			return nil, fmt.Errorf("Copilot API token is required")
		}
		return copilotRequestHeaders(token, false), nil
	}
	anthropicHeaders := func(context.Context) (map[string]string, error) {
		token := strings.TrimSpace(apiKey)
		if token == "" {
			return nil, fmt.Errorf("Copilot API token is required")
		}
		return copilotRequestHeaders(token, true), nil
	}
	session := sdkopenai.NewCustomAuthSession(sdkopenai.CustomAuthSessionConfig{
		SDKAPIKey:      "copilot-placeholder",
		RequestHeaders: openAIHeaders,
	})
	openAIProvider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		ProviderName: DefaultProviderCopilot,
		BaseURL:      baseURL,
		AuthMode:     sdkopenai.AuthModeAPIKey,
		APIMode:      apiModeForProvider(spec, DefaultProviderCopilot, "chat-completions"),
		AuthSession:  session,
	})
	// Claude models served through Copilot are routed to Copilot's
	// Anthropic-compatible /v1/messages shim instead of chat-completions. The
	// OpenAI->Anthropic chat translation reports finish_reason="tool_calls"
	// with no tool calls on plain narration turns, corrupting tool_use /
	// stop_reason semantics; the Messages shim preserves them. This mirrors
	// opencode, which routes Copilot Claude models through @ai-sdk/anthropic.
	// Set GRATEFULAGENTS_COPILOT_CLAUDE_VIA_CHAT=1 to force the legacy
	// chat-completions path.
	if copilotClaudeViaChat() {
		return openAIProvider
	}
	anthropicProvider := sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
		BaseURL:          copilotAnthropicBaseURL(baseURL),
		BearerToken:      strings.TrimSpace(apiKey),
		RequestHeaders:   anthropicHeaders,
		AdaptiveThinking: true,
	})
	return &copilotProvider{
		openai:    openAIProvider,
		anthropic: anthropicProvider,
	}
}

// copilotClaudeViaChat reports whether the legacy chat-completions path should
// be forced for Claude models on Copilot, via
// GRATEFULAGENTS_COPILOT_CLAUDE_VIA_CHAT (1/true/yes/on).
func copilotClaudeViaChat() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GRATEFULAGENTS_COPILOT_CLAUDE_VIA_CHAT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// copilotProvider routes Claude models to the Anthropic Messages API and all
// other models to the OpenAI chat-completions API, both backed by GitHub
// Copilot.
type copilotProvider struct {
	openai    agentsdk.ModelProvider
	anthropic agentsdk.ModelProvider
}

func (p *copilotProvider) GetModel(name string) (agentsdk.Model, error) {
	if isClaudeModelName(name) {
		return p.anthropic.GetModel(name)
	}
	return p.openai.GetModel(name)
}

func (p *copilotProvider) Close() error {
	var err error
	if p.anthropic != nil {
		if cerr := p.anthropic.Close(); cerr != nil {
			err = cerr
		}
	}
	if p.openai != nil {
		if cerr := p.openai.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// isClaudeModelName reports whether a model identifier names a Claude model,
// ignoring any "provider/" routing prefix (e.g. "copilot/claude-sonnet-4.5").
func isClaudeModelName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.HasPrefix(name, "claude")
}

// copilotAnthropicBaseURL derives the host root for Copilot's Anthropic
// Messages endpoint from a base URL that may carry an OpenAI path suffix. The
// Anthropic SDK appends "/v1/messages" itself, so any trailing "/chat/completions"
// or "/v1" segment must be stripped.
func copilotAnthropicBaseURL(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	b = strings.TrimSuffix(b, "/chat/completions")
	b = strings.TrimRight(b, "/")
	b = strings.TrimSuffix(b, "/v1")
	return strings.TrimRight(b, "/")
}

// Copilot client identity headers, aligned with the values used by opencode and
// the copilot-api proxy so the gateway treats requests like the VS Code Copilot
// Chat client.
const (
	copilotChatVersion      = "0.26.7"
	copilotEditorVersion    = "vscode/1.99.3"
	copilotGitHubAPIVersion = "2026-06-01"
	// copilotAnthropicBeta is the beta opencode enables for Claude models on
	// Copilot's /v1/messages shim.
	copilotAnthropicBeta = "interleaved-thinking-2025-05-14"
)

// copilotRequestHeaders returns the headers GitHub Copilot expects, matching the
// set sent by opencode/copilot-api. When forAnthropic is true it adds the
// anthropic-beta header required by Copilot's /v1/messages shim for Claude.
func copilotRequestHeaders(token string, forAnthropic bool) map[string]string {
	headers := map[string]string{
		"Authorization":          "Bearer " + strings.TrimSpace(token),
		"Copilot-Integration-Id": "vscode-chat",
		"Editor-Version":         copilotEditorVersion,
		"Editor-Plugin-Version":  "copilot-chat/" + copilotChatVersion,
		"User-Agent":             "GitHubCopilotChat/" + copilotChatVersion,
		"Openai-Intent":          "conversation-edits",
		"X-GitHub-Api-Version":   copilotGitHubAPIVersion,
		"X-Initiator":            "agent",
	}
	if forAnthropic {
		headers["anthropic-beta"] = copilotAnthropicBeta
	}
	return headers
}

func authModeForAnthropicProvider(spec ProviderSpec) string {
	authMode := strings.ToLower(strings.TrimSpace(spec.AuthMode))
	if authMode != "oauth" {
		return authMode
	}
	provider := normalizeProviderName(spec.Provider)
	if provider == DefaultProviderAnthropic || defaultProviderForSpec(spec) == DefaultProviderAnthropic {
		return authMode
	}
	return ""
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
	case DefaultProviderOpenRouter:
		return "https://openrouter.ai/api/v1"
	case DefaultProviderGemini:
		return "https://generativelanguage.googleapis.com/v1beta/openai"
	case DefaultProviderGroq:
		return "https://api.groq.com/openai/v1"
	case DefaultProviderLocal:
		return "http://localhost:11434/v1"
	case DefaultProviderCopilot:
		return defaultCopilotBaseURL
	default:
		return ""
	}
}

func defaultAPIKeyForProvider(provider string) string {
	switch normalizeProviderName(provider) {
	case DefaultProviderLocal:
		return "local-key"
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
