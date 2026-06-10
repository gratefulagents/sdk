package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// ThinkTool gives the model a dedicated scratchpad action: it records a
// thought in the transcript without obtaining new information or changing any
// state. Anthropic measured up to +54% on policy-heavy benchmarks and +1.6%
// on SWE-bench from this pattern (anthropic.com/engineering/claude-think-tool).
//
// It is most useful after tool results (analyzing output before acting),
// before policy-sensitive or irreversible actions, and in long sequential
// tasks where each step builds on the previous one.
type ThinkTool struct{}

func (t *ThinkTool) Name() string { return "think" }

func (t *ThinkTool) Description() string {
	return "Use the tool to think about something. It will not obtain new information or change anything; it only logs the thought. Use it when complex reasoning is needed between actions: analyzing tool output before acting on it, checking the task's requirements or constraints before an important or irreversible step, planning the next sequence of actions, or verifying that prior results actually satisfy the goal."
}

func (t *ThinkTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"thought": {
				"type": "string",
				"description": "Your reasoning: what you learned, what it implies, what to do next and why."
			}
		},
		"required": ["thought"]
	}`)
}

func (t *ThinkTool) IsReadOnly() bool { return true }

func (t *ThinkTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *ThinkTool) NeedsApproval() bool { return false }

func (t *ThinkTool) TimeoutSeconds() int { return 0 }

func (t *ThinkTool) Execute(_ context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Thought string `json:"thought"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Thought) == "" {
		return agentsdk.ToolResult{Content: "thought is required", IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: "(thought recorded)"}, nil
}
