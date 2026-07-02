package agent

import "encoding/json"

// RunItemType identifies the kind of item in a run.
type RunItemType int

const (
	RunItemMessage RunItemType = iota
	RunItemToolCall
	RunItemToolOutput
	RunItemHandoffCall
	RunItemHandoffOutput
	RunItemReasoning
	RunItemToolApproval
	RunItemCompaction
)

// RunItem is a single item produced during a run.
// Exactly one of the data fields is non-nil, matching Type.
type RunItem struct {
	Type          RunItemType
	Agent         *Agent
	Message       *MessageOutput
	ToolCall      *ToolCallData
	ToolOutput    *ToolOutputData
	HandoffCall   *HandoffCallData
	HandoffOutput *HandoffOutputData
	Reasoning     *ReasoningData
	Compaction    *CompactionData
	ToolApproval  *ToolApprovalData
}

// MessageOutput holds text content from the model.
type MessageOutput struct {
	Text string `json:"text"`
	// Images carries inbound image attachments for multimodal user messages.
	// Outbound model messages leave this empty.
	Images []ImageAttachment `json:"images,omitempty"`
}

// ImageAttachment is a base64-encoded image attached to a user message.
type ImageAttachment struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ToolCallData represents a tool invocation by the model.
type ToolCallData struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolOutputData holds the result of a tool execution.
type ToolOutputData struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// HandoffCallData records an agent-to-agent handoff request.
type HandoffCallData struct {
	FromAgent string `json:"from_agent"`
	ToAgent   string `json:"to_agent"`
}

// HandoffOutputData records the completion of a handoff.
type HandoffOutputData struct {
	FromAgent string `json:"from_agent"`
	ToAgent   string `json:"to_agent"`
}

// ReasoningData holds model reasoning/thinking content.
type ReasoningData struct {
	ID               string `json:"id,omitempty"`
	Text             string `json:"text,omitempty"`
	Signature        string `json:"signature,omitempty"`
	RedactedData     string `json:"redacted_data,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

// CompactionData holds an opaque provider compaction item.
type CompactionData struct {
	ID               string `json:"id,omitempty"`
	Content          string `json:"content,omitempty"` // provider summary of compacted context
	EncryptedContent string `json:"encrypted_content,omitempty"`
	CreatedBy        string `json:"created_by,omitempty"`
}

// ToolApprovalData records a tool approval request or response.
type ToolApprovalData struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
	CallID   string          `json:"call_id"`
	Approved bool            `json:"approved"`
}

// ItemHelpers provides utility functions for working with RunItem slices.
var Items = ItemHelpers{}

// ItemHelpers contains methods for extracting data from RunItem slices.
type ItemHelpers struct{}

// ExtractText concatenates all message text from the items.
func (ItemHelpers) ExtractText(items []RunItem) string {
	var result string
	for _, item := range items {
		if item.Type == RunItemMessage && item.Message != nil {
			if result != "" {
				result += "\n"
			}
			result += item.Message.Text
		}
	}
	return result
}

// ExtractToolCalls returns all tool call data from the items.
func (ItemHelpers) ExtractToolCalls(items []RunItem) []ToolCallData {
	var calls []ToolCallData
	for _, item := range items {
		if item.Type == RunItemToolCall && item.ToolCall != nil {
			calls = append(calls, *item.ToolCall)
		}
	}
	return calls
}

// ExtractLastText returns the text from the last message item, or empty string.
func (ItemHelpers) ExtractLastText(items []RunItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == RunItemMessage && items[i].Message != nil {
			return items[i].Message.Text
		}
	}
	return ""
}
