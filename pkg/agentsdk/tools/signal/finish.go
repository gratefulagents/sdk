package signal

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// FinishSink is notified when the finish tool is called.
type FinishSink interface {
	Finish(ctx context.Context, summary string) error
}

// FinishTool signals that the run has completed.
type FinishTool struct {
	Sink FinishSink
}

func (t *FinishTool) Name() string { return "finish" }

func (t *FinishTool) Description() string {
	return `Signal that you have completed all work for this run.

Call this tool when you are done with the task. Include a brief summary of what was accomplished.`
}

func (t *FinishTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {
				"type": "string",
				"description": "Brief summary of what was accomplished"
			}
		}
	}`)
}

func (t *FinishTool) IsReadOnly() bool { return false }

func (t *FinishTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *FinishTool) NeedsApproval() bool { return false }

func (t *FinishTool) TimeoutSeconds() int { return 0 }

func (t *FinishTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if t.Sink != nil {
		if err := t.Sink.Finish(ctx, in.Summary); err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to mark run as completed: %v", err), IsError: true}, nil
		}
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Run completed.\n\nSummary: %s", in.Summary), ShouldPause: true}, nil
}
