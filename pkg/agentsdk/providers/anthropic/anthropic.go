package anthropic

import (
	"encoding/json"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

type APIError = internalanthropic.APIError
type CacheControl = internalanthropic.CacheControl
type Client = internalanthropic.Client
type ClientOption = internalanthropic.Option
type ContentBlock = internalanthropic.ContentBlock
type CreateMessageRequest = internalanthropic.CreateMessageRequest
type CreateMessageResponse = internalanthropic.CreateMessageResponse
type DeltaBlock = internalanthropic.DeltaBlock
type Metadata = internalanthropic.Metadata
type ModelPricing = internalanthropic.ModelPricing
type Model = AnthropicModel
type Provider = AnthropicProvider
type RequestError = internalanthropic.RequestError
type Role = internalanthropic.Role
type StopReason = internalanthropic.StopReason
type StreamEvent = internalanthropic.StreamEvent
type StreamEventType = internalanthropic.StreamEventType
type StreamReader = internalanthropic.StreamReader
type SystemBlock = internalanthropic.SystemBlock
type ThinkingConfig = internalanthropic.ThinkingConfig
type ToolChoice = internalanthropic.ToolChoice
type ToolDefinition = internalanthropic.ToolDefinition
type Usage = internalanthropic.Usage

const (
	RoleUser      = internalanthropic.RoleUser
	RoleAssistant = internalanthropic.RoleAssistant

	StopReasonEndTurn   = internalanthropic.StopReasonEndTurn
	StopReasonToolUse   = internalanthropic.StopReasonToolUse
	StopReasonMaxTokens = internalanthropic.StopReasonMaxTokens

	EventMessageStart      = internalanthropic.EventMessageStart
	EventContentBlockStart = internalanthropic.EventContentBlockStart
	EventContentBlockDelta = internalanthropic.EventContentBlockDelta
	EventContentBlockStop  = internalanthropic.EventContentBlockStop
	EventMessageDelta      = internalanthropic.EventMessageDelta
	EventMessageStop       = internalanthropic.EventMessageStop
	EventPing              = internalanthropic.EventPing
	EventError             = internalanthropic.EventError
)

func NewProvider() *Provider {
	return NewAnthropicProvider()
}

func NewProviderWithConfig(cfg ProviderConfig) *Provider {
	return NewAnthropicProviderWithConfig(cfg)
}

func NewModelWithClient(client *Client) *Model {
	return NewAnthropicModelWithClient(client)
}

func NewClient(apiKey string, opts ...ClientOption) *Client {
	return internalanthropic.NewClient(apiKey, opts...)
}

func WithBaseURL(baseURL string) ClientOption {
	return internalanthropic.WithBaseURL(baseURL)
}

func WithMaxConcurrent(n int) ClientOption {
	return internalanthropic.WithMaxConcurrent(n)
}

func NewTextBlock(text string) ContentBlock {
	return internalanthropic.NewTextBlock(text)
}

func NewThinkingBlock(thinking string) ContentBlock {
	return internalanthropic.NewThinkingBlock(thinking)
}

func NewRedactedThinkingBlock(data string) ContentBlock {
	return internalanthropic.NewRedactedThinkingBlock(data)
}

func NewCompactionBlock(id, encryptedContent, createdBy string) ContentBlock {
	return internalanthropic.NewCompactionBlock(id, encryptedContent, createdBy)
}

func NewToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return internalanthropic.NewToolUseBlock(id, name, input)
}

func NewToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return internalanthropic.NewToolResultBlock(toolUseID, content, isError)
}

func ModelBetas(model string) []string {
	return internalanthropic.ModelBetas(model)
}

func CalculateCost(model string, usage agentsdk.Usage) float64 {
	return internalanthropic.CalculateCost(model, Usage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
		CacheCreationInputTokens: usage.CacheCreateTokens,
	})
}

var _ agentsdk.ModelProvider = (*Provider)(nil)
var _ agentsdk.Model = (*Model)(nil)
