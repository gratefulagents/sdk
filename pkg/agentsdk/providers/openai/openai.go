package openai

import (
	"context"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	internalopenai "github.com/gratefulagents/sdk/internal/openai"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

type AuthMode = internalopenai.AuthMode
type AuthSession = internalopenai.OpenAIAuthSession
type Client = internalopenai.Client
type ClientOption = internalopenai.Option
type CompactConversationResponse = internalopenai.CompactConversationResponse
type ModelMetadata = internalopenai.ModelMetadata
type ModelPricing = internalopenai.ModelPricing
type OAuthSessionConfig = internalopenai.OAuthSessionConfig
type Model = OpenAIModel
type OpenAIAuthSession = internalopenai.OpenAIAuthSession
type Provider = OpenAIProvider
type RequestError = internalopenai.RequestError

const (
	AuthModeAPIKey = internalopenai.AuthModeAPIKey
	AuthModeOAuth  = internalopenai.AuthModeOAuth

	DefaultChatModel          = internalopenai.DefaultChatModel
	DefaultChatMiniModel      = internalopenai.DefaultChatMiniModel
	DefaultCodexClientVersion = internalopenai.DefaultCodexClientVersion
)

func NewProvider() *Provider {
	return NewOpenAIProvider()
}

func NewProviderWithBaseURL(baseURL string) *Provider {
	return NewOpenAIProviderWithBaseURL(baseURL)
}

func NewProviderWithConfig(cfg ProviderConfig) *Provider {
	return NewOpenAIProviderWithConfig(cfg)
}

func NewModelWithClient(client *Client) *Model {
	return NewOpenAIModelWithClient(client)
}

func NewClient(apiKey string, opts ...ClientOption) *Client {
	return internalopenai.NewClient(apiKey, opts...)
}

func NewClientWithAuthSession(session *AuthSession, opts ...ClientOption) *Client {
	return internalopenai.NewClientWithAuthSession(session, opts...)
}

func NewAPIKeyAuthSession(apiKey string) *AuthSession {
	return internalopenai.NewAPIKeyAuthSession(apiKey)
}

func NewOAuthAuthSessionFromFile(authJSONPath, accountIDPath string) (*AuthSession, error) {
	return internalopenai.NewOAuthAuthSessionFromFile(authJSONPath, accountIDPath)
}

func NewOAuthAuthSessionFromSecretData(authJSON []byte, accountIDOverride string) (*AuthSession, error) {
	return internalopenai.NewOAuthAuthSessionFromSecretData(authJSON, accountIDOverride)
}

func NewOAuthAuthSessionFromConfig(cfg OAuthSessionConfig) (*AuthSession, error) {
	return internalopenai.NewOAuthAuthSessionFromConfig(cfg)
}

func NormalizeAuthMode(mode string) AuthMode {
	return internalopenai.NormalizeAuthMode(mode)
}

func CodexClientVersion() string {
	return internalopenai.CodexClientVersion()
}

func WithBaseURL(baseURL string) ClientOption {
	return internalopenai.WithBaseURL(baseURL)
}

func WithMaxConcurrent(n int) ClientOption {
	return internalopenai.WithMaxConcurrent(n)
}

func WithAPIMode(mode string) ClientOption {
	return internalopenai.WithAPIMode(mode)
}

func WithAuthSession(session *AuthSession) ClientOption {
	return internalopenai.WithAuthSession(session)
}

func SupportsChatCompletions(model string) bool {
	return internalopenai.SupportsChatCompletions(model)
}

func ValidateChatCompletionsModel(model string) error {
	return internalopenai.ValidateChatCompletionsModel(model)
}

func IsChatGPTBackendBaseURL(raw string) bool {
	return internalopenai.IsChatGPTBackendBaseURL(raw)
}

func FetchModelMetadata(ctx context.Context, baseURL string, session *AuthSession) ([]ModelMetadata, error) {
	return internalopenai.FetchModelMetadata(ctx, baseURL, session)
}

func FetchModelMetadataByID(ctx context.Context, baseURL string, session *AuthSession) (map[string]ModelMetadata, error) {
	return internalopenai.FetchModelMetadataByID(ctx, baseURL, session)
}

func EstimateCost(model string, usage agentsdk.Usage) (float64, bool) {
	return internalopenai.EstimateCost(model, internalanthropic.Usage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
		CacheCreationInputTokens: usage.CacheCreateTokens,
	})
}

func CalculateCost(model string, usage agentsdk.Usage) float64 {
	cost, _ := EstimateCost(model, usage)
	return cost
}

var _ agentsdk.ModelProvider = (*Provider)(nil)
var _ agentsdk.Model = (*Model)(nil)
