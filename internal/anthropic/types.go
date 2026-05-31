package anthropic

import "encoding/json"

// Role for messages.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message in a conversation.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is a union type for message content.
// The Type field determines which other fields are relevant.
type ContentBlock struct {
	Type string `json:"type"`

	// TextBlock fields (type="text").
	Text  string `json:"text,omitempty"`
	Phase string `json:"phase,omitempty"`

	// ThinkingBlock fields (type="thinking").
	Thinking         string `json:"thinking,omitempty"`
	Signature        string `json:"signature,omitempty"`
	Data             string `json:"data,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`

	// ToolUseBlock fields (type="tool_use").
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	CreatedBy string          `json:"created_by,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`

	// ToolResultBlock fields (type="tool_result").
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"` // for tool_result text
	IsError   bool   `json:"is_error,omitempty"`

	// ImageBlock fields (type="image").
	Source *ImageSource `json:"source,omitempty"`

	// CacheControl for prompt caching.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ImageSource is a base64-encoded image payload for an image content block.
type ImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// NewTextBlock creates a text content block.
func NewTextBlock(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// NewThinkingBlock creates a thinking content block.
func NewThinkingBlock(thinking string) ContentBlock {
	return ContentBlock{Type: "thinking", Thinking: thinking}
}

// NewRedactedThinkingBlock creates a redacted_thinking content block.
func NewRedactedThinkingBlock(data string) ContentBlock {
	return ContentBlock{Type: "redacted_thinking", Data: data}
}

// NewCompactionBlock creates an opaque provider compaction content block.
func NewCompactionBlock(id, encryptedContent, createdBy string) ContentBlock {
	return ContentBlock{Type: "compaction", ID: id, EncryptedContent: encryptedContent, CreatedBy: createdBy}
}

// NewToolUseBlock creates a tool_use content block.
func NewToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: "tool_use", ID: id, Name: name, Input: input}
}

// NewToolResultBlock creates a tool_result content block.
func NewToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{Type: "tool_result", ToolUseID: toolUseID, Content: content, IsError: isError}
}

// NewImageBlock creates an image content block from base64-encoded data.
func NewImageBlock(mediaType, base64Data string) ContentBlock {
	return ContentBlock{Type: "image", Source: &ImageSource{Type: "base64", MediaType: mediaType, Data: base64Data}}
}

// CacheControl for prompt caching.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// Usage tracks token counts from the API.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// ToolDefinition for the API request.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// SystemBlock for system prompt segments.
type SystemBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// StopReason from the API response.
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonToolUse   StopReason = "tool_use"
	StopReasonMaxTokens StopReason = "max_tokens"
)
