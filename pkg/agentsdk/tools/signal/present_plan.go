package signal

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// PresentPlanTool signals the agent to pause and present a plan to the user
// with structured quick-action buttons.
type PresentPlanTool struct{}

func (t *PresentPlanTool) Name() string { return "present_plan" }

func (t *PresentPlanTool) Description() string {
	return `Present the finalized plan to the user for approval with action buttons.

Call this when a plan is ready for user review and approval. The user will see the
summary along with clickable action buttons.`
}

func (t *PresentPlanTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {
				"type": "string",
				"description": "A brief summary of the plan for display to the user"
			},
			"actions": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "string", "description": "Unique action identifier"},
						"label": {"type": "string", "description": "Button label shown to the user"},
						"mode": {"type": "string", "description": "Target mode to switch to when clicked"},
						"style": {
							"type": "string",
							"enum": ["primary", "secondary", "destructive"],
							"description": "Button style"
						}
					},
					"required": ["id", "label"]
				},
				"description": "List of action buttons to show the user"
			},
			"recommended": {
				"type": "string",
				"description": "ID of the recommended action"
			}
		},
		"required": ["summary", "actions"]
	}`)
}

func (t *PresentPlanTool) IsReadOnly() bool { return true }

func (t *PresentPlanTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *PresentPlanTool) NeedsApproval() bool { return false }

func (t *PresentPlanTool) TimeoutSeconds() int { return 0 }

// PresentPlanAction represents a single action button in a present_plan call.
type PresentPlanAction = agentsdk.QuickAction

type presentPlanInput struct {
	Summary     string              `json:"summary"`
	Actions     []PresentPlanAction `json:"actions"`
	Recommended string              `json:"recommended"`
}

func (t *PresentPlanTool) Execute(_ context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in presentPlanInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.Summary == "" {
		return agentsdk.ToolResult{Content: "summary is required", IsError: true}, nil
	}
	if len(in.Actions) == 0 {
		return agentsdk.ToolResult{Content: "at least one action is required", IsError: true}, nil
	}
	data, _ := json.Marshal(in)
	return agentsdk.ToolResult{Content: fmt.Sprintf("Plan presented to user.\n%s", string(data))}, nil
}
