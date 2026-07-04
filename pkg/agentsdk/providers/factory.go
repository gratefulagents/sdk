package providers

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
	sdkoauth "github.com/gratefulagents/sdk/pkg/agentsdk/providers/oauth"
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
	// CopilotOAuthPath, when set for the copilot provider, points at Copilot
	// auth JSON containing the long-lived GitHub OAuth token. The provider
	// then self-refreshes the short-lived Copilot API token per request
	// (re-reading the file to pick up external rotations) instead of pinning
	// the startup token, which GitHub expires after ~25–30 minutes.
	CopilotOAuthPath string
	ProviderAPIKeys  map[string]string
	ProviderBaseURLs map[string]string
	ProviderAPIModes map[string]string
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

	// CopilotOAuthPath mirrors ProviderSpec.CopilotOAuthPath for routes whose
	// Provider is "copilot": auth JSON with the GitHub OAuth token enabling
	// per-request self-refresh of the short-lived Copilot API token.
	CopilotOAuthPath string
}

var openAICompatibleProviderNames = []string{
	DefaultProviderOpenRouter,
	DefaultProviderGemini,
	DefaultProviderGroq,
	DefaultProviderLocal,
}

const defaultCopilotBaseURL = sdkoauth.CopilotDefaultBaseURL

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
			BaseURL:       baseURLForProvider(spec, DefaultProviderAnthropic),
			APIKey:        apiKeyForProvider(spec, DefaultProviderAnthropic),
			AuthMode:      authModeForAnthropicProvider(spec),
			PromptCaching: true,
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
	authMode := authModeForOpenAIProvider(spec)
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
		APIMode:     apiModeForProvider(spec, DefaultProviderOpenAI, ""),
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
		BaseURL:       baseURLForProvider(spec, DefaultProviderAnthropic),
		APIKey:        apiKeyForProvider(spec, DefaultProviderAnthropic),
		AuthMode:      authModeForAnthropicProvider(spec),
		PromptCaching: true,
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
		CopilotOAuthPath:         route.CopilotOAuthPath,
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
	// With an auth-JSON path the provider self-refreshes: GitHub mints Copilot
	// API tokens with a ~25–30 minute lifetime, so long-lived processes must
	// re-exchange the GitHub OAuth token instead of pinning the startup token.
	var tokenSource *sdkoauth.CopilotTokenSource
	if authPath := strings.TrimSpace(spec.CopilotOAuthPath); authPath != "" {
		tokenSource = sdkoauth.NewCopilotTokenSource(sdkoauth.CopilotTokenSourceConfig{
			Auth:     sdkoauth.CopilotAuth{Token: strings.TrimSpace(apiKey)},
			AuthPath: authPath,
			Refresh: sdkoauth.RefreshConfig{
				CopilotEditorVersion:       copilotEditorVersion,
				CopilotEditorPluginVersion: "copilot-chat/" + copilotChatVersion,
				CopilotUserAgent:           "GitHubCopilotChat/" + copilotChatVersion,
			},
		})
	}
	currentToken := func(ctx context.Context) (string, error) {
		if tokenSource != nil {
			return tokenSource.Token(ctx)
		}
		token := strings.TrimSpace(apiKey)
		if token == "" {
			return "", fmt.Errorf("Copilot API token is required")
		}
		return token, nil
	}
	initialToken := strings.TrimSpace(apiKey)
	if initialToken == "" && tokenSource != nil {
		initialToken = tokenSource.CurrentToken()
	}
	configuredBaseURL := baseURLForProvider(spec, DefaultProviderCopilot)
	baseURL := configuredBaseURL
	if baseURL == "" || isDefaultCopilotBaseURL(baseURL) {
		baseURL = firstNonEmpty(sdkoauth.CopilotAPIBaseURLFromToken(initialToken), baseURL, defaultCopilotBaseURL)
	}
	openAIHeaders := func(ctx context.Context) (map[string]string, error) {
		token, err := currentToken(ctx)
		if err != nil {
			return nil, err
		}
		return copilotRequestHeaders(token, false), nil
	}
	anthropicHeaders := func(ctx context.Context) (map[string]string, error) {
		token, err := currentToken(ctx)
		if err != nil {
			return nil, err
		}
		return copilotRequestHeaders(token, true), nil
	}
	session := sdkopenai.NewCustomAuthSession(sdkopenai.CustomAuthSessionConfig{
		SDKAPIKey:      "copilot-placeholder",
		RequestHeaders: openAIHeaders,
	})
	chatProvider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		ProviderName: DefaultProviderCopilot,
		BaseURL:      baseURL,
		AuthMode:     sdkopenai.AuthModeAPIKey,
		APIMode:      "chat-completions",
		AuthSession:  session,
	})
	if copilotForceChatCompletions() {
		return chatProvider
	}
	responsesProvider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		ProviderName: DefaultProviderCopilot,
		BaseURL:      baseURL,
		AuthMode:     sdkopenai.AuthModeAPIKey,
		APIMode:      "responses",
		AuthSession:  session,
	})
	messagesProvider := sdkanthropic.NewProviderWithConfig(sdkanthropic.ProviderConfig{
		BaseURL: copilotAnthropicBaseURL(baseURL),
		// The per-request header hook overwrites Authorization with the current
		// token; the static bearer only satisfies the client's credential check.
		BearerToken:      firstNonEmpty(initialToken, "copilot-placeholder"),
		RequestHeaders:   anthropicHeaders,
		AdaptiveThinking: true,
		// Verified against the Copilot /v1/messages shim: cache_control
		// breakpoints bill cache reads at 0.1x, and oversized max_tokens is
		// capped (not rejected), so the 64000 streaming ceiling from /models
		// is a safe default for every Claude model it serves.
		PromptCaching:    true,
		DefaultMaxTokens: 64000,
	})
	return &copilotRoutingProvider{
		chat:      chatProvider,
		responses: responsesProvider,
		messages:  messagesProvider,
	}
}

// copilotForceChatCompletions restores the previous Copilot routing path for
// debugging or rollback.
func copilotForceChatCompletions() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GRATEFULAGENTS_COPILOT_CHAT_COMPLETIONS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// copilotRoutingProvider mirrors Copilot clients that choose the wire protocol
// from /models supported_endpoints: messages first, then responses, then chat.
// The SDK does not fetch /models in the hot path, so it uses the same fallback
// heuristic used by several clients when metadata is unavailable.
type copilotRoutingProvider struct {
	chat      agentsdk.ModelProvider
	responses agentsdk.ModelProvider
	messages  agentsdk.ModelProvider
}

func (p *copilotRoutingProvider) GetModel(name string) (agentsdk.Model, error) {
	if p == nil {
		return nil, fmt.Errorf("Copilot provider is not configured")
	}
	normalized := normalizeCopilotModelName(name)
	var provider agentsdk.ModelProvider
	switch {
	case copilotModelUsesMessages(normalized):
		provider = p.messages
	case copilotModelUsesResponses(normalized):
		provider = p.responses
	default:
		provider = p.chat
	}
	if provider == nil {
		return nil, fmt.Errorf("Copilot provider is not configured")
	}
	model, err := provider.GetModel(normalized)
	if err != nil {
		return nil, err
	}
	return &copilotModel{Model: model}, nil
}

func (p *copilotRoutingProvider) Close() error {
	if p == nil {
		return nil
	}
	var err error
	for _, provider := range []agentsdk.ModelProvider{p.chat, p.responses, p.messages} {
		if provider == nil {
			continue
		}
		if closeErr := provider.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

type copilotModel struct {
	agentsdk.Model
}

func (m *copilotModel) GetResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelResponse, error) {
	req.Model = normalizeCopilotModelName(req.Model)
	return m.Model.GetResponse(ctx, req)
}

func (m *copilotModel) StreamResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelStream, error) {
	req.Model = normalizeCopilotModelName(req.Model)
	return m.Model.StreamResponse(ctx, req)
}

func (m *copilotModel) Provider() string {
	return DefaultProviderCopilot
}

func normalizeCopilotModelName(name string) string {
	if prefix, bare := agentsdk.ParseModelPrefix(name); strings.EqualFold(strings.TrimSpace(prefix), DefaultProviderCopilot) {
		return strings.TrimSpace(bare)
	}
	return strings.TrimSpace(name)
}

func isDefaultCopilotBaseURL(baseURL string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch strings.ToLower(trimmed) {
	case "https://api.githubcopilot.com", "https://api.individual.githubcopilot.com":
		return true
	default:
		return false
	}
}

func copilotModelUsesMessages(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(normalized, "claude-")
}

func copilotModelUsesResponses(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(normalized, "gpt-5") || strings.Contains(normalized, "codex")
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
	copilotChatVersion      = "0.35.0"
	copilotEditorVersion    = "vscode/1.107.0"
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
		"X-Initiator":            "user",
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

// authModeForOpenAIProvider scopes a top-level AuthMode=oauth to the OpenAI
// provider, mirroring authModeForAnthropicProvider. Without this, a multi spec
// whose OAuth setting targets another default provider would push the
// always-registered OpenAI leg onto the Codex OAuth backend.
func authModeForOpenAIProvider(spec ProviderSpec) sdkopenai.AuthMode {
	authMode := sdkopenai.NormalizeAuthMode(spec.AuthMode)
	if authMode != sdkopenai.AuthModeOAuth {
		return authMode
	}
	provider := normalizeProviderName(spec.Provider)
	if provider == DefaultProviderOpenAI || defaultProviderForSpec(spec) == DefaultProviderOpenAI {
		return authMode
	}
	return sdkopenai.NormalizeAuthMode("")
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
