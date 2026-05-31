package signal

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// AskUserQuestionTool signals the agent to pause and ask the user a question.
type AskUserQuestionTool struct{}

func (t *AskUserQuestionTool) Name() string { return "AskUserQuestion" }

func (t *AskUserQuestionTool) Description() string {
	return `Ask the user a question and wait for their response.

When asking the user a question, prefer providing choices when possible. This helps
the user respond quickly with a single click. Only use freeform (no choices) when the
answer truly cannot be predicted.`
}

func (t *AskUserQuestionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"question": {
				"type": "string",
				"description": "The question to ask the user"
			},
			"choices": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional list of choices for the user to pick from. Prefer providing choices when possible."
			},
			"allow_freeform": {
				"type": "boolean",
				"description": "Whether to allow freeform text input in addition to choices. Defaults to true.",
				"default": true
			}
		},
		"required": ["question"]
	}`)
}

func (t *AskUserQuestionTool) IsReadOnly() bool { return true }

func (t *AskUserQuestionTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *AskUserQuestionTool) NeedsApproval() bool { return false }

func (t *AskUserQuestionTool) TimeoutSeconds() int { return 0 }

func (t *AskUserQuestionTool) Execute(_ context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Question      string   `json:"question"`
		Choices       []string `json:"choices"`
		AllowFreeform *bool    `json:"allow_freeform"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}

	if len(in.Choices) > 0 {
		allowFreeform := true
		if in.AllowFreeform != nil {
			allowFreeform = *in.AllowFreeform
		}
		resp := struct {
			Question      string   `json:"question"`
			Choices       []string `json:"choices"`
			AllowFreeform bool     `json:"allow_freeform"`
		}{
			Question:      in.Question,
			Choices:       in.Choices,
			AllowFreeform: allowFreeform,
		}
		data, _ := json.Marshal(resp)
		return agentsdk.ToolResult{Content: string(data)}, nil
	}

	return agentsdk.ToolResult{Content: in.Question}, nil
}
