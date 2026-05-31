package anthropic

import (
	"encoding/json"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// CreateMessageRequest for POST /v1/messages.
type CreateMessageRequest struct {
	Model         string           `json:"model"`
	MaxTokens     int              `json:"max_tokens"`
	Messages      []Message        `json:"messages"`
	System        []SystemBlock    `json:"system,omitempty"`
	Tools         []ToolDefinition `json:"tools,omitempty"`
	Stream        bool             `json:"stream"`
	Metadata      *Metadata        `json:"metadata,omitempty"`
	Thinking      *ThinkingConfig  `json:"thinking,omitempty"`
	TextVerbosity string           `json:"text_verbosity,omitempty"`
	ToolChoice    *ToolChoice      `json:"tool_choice,omitempty"`
	Betas         []string         `json:"-"` // Converted to SDK beta headers
	OutputSchema  *OutputSchema    `json:"-"`

	// OpenAI Responses shim only: token threshold for server-side compaction.
	CompactionThreshold int `json:"-"`
}

// Metadata for the API request.
type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

// OutputSchema carries host SDK structured-output settings into providers that
// support native response-format enforcement.
type OutputSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

// ThinkingConfig enables extended thinking.
type ThinkingConfig struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// ToolChoice controls tool selection behavior.
type ToolChoice struct {
	Type string `json:"type"`           // "auto", "any", "tool"
	Name string `json:"name,omitempty"` // only for type "tool"
}

// toSDKParams converts our request to SDK BetaMessageNewParams + request options for betas.
func toSDKParams(r *CreateMessageRequest) (sdk.BetaMessageNewParams, []option.RequestOption) {
	params := sdk.BetaMessageNewParams{
		Model:     sdk.Model(r.Model),
		MaxTokens: int64(r.MaxTokens),
	}

	// Convert messages.
	for _, msg := range r.Messages {
		sdkMsg := sdk.BetaMessageParam{
			Role: sdk.BetaMessageParamRole(msg.Role),
		}
		for _, block := range msg.Content {
			sdkMsg.Content = append(sdkMsg.Content, toSDKContentBlock(block))
		}
		params.Messages = append(params.Messages, sdkMsg)
	}

	// Convert system prompt.
	for _, sys := range r.System {
		tb := sdk.BetaTextBlockParam{
			Text: sys.Text,
		}
		if sys.CacheControl != nil {
			tb.CacheControl = sdk.BetaCacheControlEphemeralParam{
				Type: "ephemeral",
			}
		}
		params.System = append(params.System, tb)
	}

	// Convert tools.
	for _, tool := range r.Tools {
		var props map[string]interface{}
		_ = json.Unmarshal(tool.InputSchema, &props)

		var required []string
		if rawRequired, ok := props["required"].([]interface{}); ok {
			for _, item := range rawRequired {
				if name, ok := item.(string); ok {
					required = append(required, name)
				}
			}
		}

		params.Tools = append(params.Tools, sdk.BetaToolUnionParam{
			OfTool: &sdk.BetaToolParam{
				Name:        tool.Name,
				Description: sdk.String(tool.Description),
				InputSchema: sdk.BetaToolInputSchemaParam{
					Properties: props["properties"],
					Required:   required,
				},
			},
		})
	}

	// Convert thinking config.
	if r.Thinking != nil && r.Thinking.Type == "enabled" {
		params.Thinking = sdk.BetaThinkingConfigParamUnion{
			OfEnabled: &sdk.BetaThinkingConfigEnabledParam{
				BudgetTokens: int64(r.Thinking.BudgetTokens),
			},
		}
	}

	// Convert betas to SDK AnthropicBeta values.
	for _, b := range r.Betas {
		params.Betas = append(params.Betas, sdk.AnthropicBeta(b))
	}

	return params, nil
}

// toSDKContentBlock converts our ContentBlock to SDK's BetaContentBlockParamUnion.
func toSDKContentBlock(block ContentBlock) sdk.BetaContentBlockParamUnion {
	switch block.Type {
	case "text":
		b := sdk.BetaTextBlockParam{
			Text: block.Text,
		}
		if block.CacheControl != nil {
			b.CacheControl = sdk.BetaCacheControlEphemeralParam{
				Type: "ephemeral",
			}
		}
		return sdk.BetaContentBlockParamUnion{OfText: &b}
	case "tool_use":
		var input interface{}
		if block.Input != nil {
			_ = json.Unmarshal(block.Input, &input)
		}
		return sdk.BetaContentBlockParamUnion{
			OfToolUse: &sdk.BetaToolUseBlockParam{
				ID:    block.ID,
				Name:  block.Name,
				Input: input,
			},
		}
	case "tool_result":
		result := &sdk.BetaToolResultBlockParam{
			ToolUseID: block.ToolUseID,
			IsError:   sdk.Bool(block.IsError),
		}
		if block.Content != "" {
			result.Content = []sdk.BetaToolResultBlockParamContentUnion{
				{OfText: &sdk.BetaTextBlockParam{Text: block.Content}},
			}
		}
		return sdk.BetaContentBlockParamUnion{OfToolResult: result}
	case "thinking":
		return sdk.BetaContentBlockParamUnion{
			OfThinking: &sdk.BetaThinkingBlockParam{
				Signature: block.Signature,
				Thinking:  block.Thinking,
			},
		}
	case "redacted_thinking":
		return sdk.BetaContentBlockParamUnion{
			OfRedactedThinking: &sdk.BetaRedactedThinkingBlockParam{
				Data: block.Data,
			},
		}
	case "image":
		if block.Source == nil {
			return sdk.BetaContentBlockParamUnion{OfText: &sdk.BetaTextBlockParam{Text: ""}}
		}
		return sdk.BetaContentBlockParamUnion{
			OfImage: &sdk.BetaImageBlockParam{
				Source: sdk.BetaImageBlockParamSourceUnion{
					OfBase64: &sdk.BetaBase64ImageSourceParam{
						Data:      block.Source.Data,
						MediaType: sdk.BetaBase64ImageSourceMediaType(block.Source.MediaType),
					},
				},
			},
		}
	default:
		return sdk.BetaContentBlockParamUnion{
			OfText: &sdk.BetaTextBlockParam{Text: block.Text},
		}
	}
}

// CreateMessageResponse from the API (non-streaming and assembled from stream).
type CreateMessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"` // "message"
	Role       Role           `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason StopReason     `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// StreamEventType identifies the type of a streaming SSE event.
type StreamEventType string

const (
	EventMessageStart      StreamEventType = "message_start"
	EventContentBlockStart StreamEventType = "content_block_start"
	EventContentBlockDelta StreamEventType = "content_block_delta"
	EventContentBlockStop  StreamEventType = "content_block_stop"
	EventMessageDelta      StreamEventType = "message_delta"
	EventMessageStop       StreamEventType = "message_stop"
	EventPing              StreamEventType = "ping"
	EventError             StreamEventType = "error"
)

// StreamEvent is a parsed streaming event.
type StreamEvent struct {
	Type StreamEventType `json:"type"`

	// message_start
	Message *CreateMessageResponse `json:"message,omitempty"`

	// content_block_start
	Index        int           `json:"index,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`

	// content_block_delta and message_delta
	Delta *DeltaBlock `json:"delta,omitempty"`

	// message_delta: final usage
	Usage *Usage `json:"usage,omitempty"`

	// error
	Error *APIError `json:"error,omitempty"`
}

// DeltaBlock holds incremental content in a streaming delta event.
type DeltaBlock struct {
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	Thinking         string `json:"thinking,omitempty"`
	Signature        string `json:"signature,omitempty"`
	PartialJSON      string `json:"partial_json,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
	StopReason       string `json:"stop_reason,omitempty"`
	Usage            *Usage `json:"usage,omitempty"`
}

// APIError from the API.
type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Beta header constants matching Claude Code's beta system.
const (
	BetaClaudeCode        = "claude-code-20250219"
	BetaInterleavedThink  = "interleaved-thinking-2025-05-14"
	BetaPromptCacheScope  = "prompt-caching-scope-2026-01-05"
	BetaContextManagement = "context-management-2025-06-27"
)

// ModelBetas returns the beta headers for a given model.
func ModelBetas(model string) []string {
	betas := []string{BetaClaudeCode}

	if !isClaude3Model(model) {
		betas = append(betas, BetaInterleavedThink)
	}

	betas = append(betas, BetaContextManagement)
	betas = append(betas, BetaPromptCacheScope)

	return betas
}

func isClaude3Model(model string) bool {
	return strings.Contains(model, "claude-3-")
}
