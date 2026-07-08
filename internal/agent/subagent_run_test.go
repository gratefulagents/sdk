package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPartialProgressTail(t *testing.T) {
	if got := partialProgressTail(nil); got != "" {
		t.Errorf("nil result should give empty tail, got %q", got)
	}
	result := &RunResult{NewItems: []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "first finding"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "echo"}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "c1", Content: "ok"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "latest finding: root cause in loop.go"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "[SYSTEM] Turn budget warning: wrap up"}},
	}}
	if got := partialProgressTail(result); got != "latest finding: root cause in loop.go" {
		t.Errorf("expected last non-system assistant text, got %q", got)
	}
	empty := &RunResult{NewItems: []RunItem{
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "echo"}},
	}}
	if got := partialProgressTail(empty); got != "" {
		t.Errorf("no assistant text should give empty tail, got %q", got)
	}
}

// A child that fails on a model/API error (not just budget exhaustion) hands
// back its accumulated run state; the outcome must surface the findings
// gathered before the failure so the parent never receives a bare error for
// minutes of completed work.
func TestRunSubAgentOnceModelFailureSurfacesPartialProgress(t *testing.T) {
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{
				{Type: RunItemMessage, Message: &MessageOutput{Text: "researched 12 sources; key insight: durable task records"}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "echo", Input: json.RawMessage(`"x"`)}},
			}},
		},
		errors: []error{nil, errors.New("model call exceeded per-attempt timeout")},
	}
	outcome := runSubAgentOnce(context.Background(), subAgentRunSpec{
		Runner:    NewRunnerWithModel(model),
		Agent:     &Agent{Name: "researcher", Tools: []Tool{echoTool}},
		Message:   "research the beat",
		TaskID:    "task_partial",
		RunConfig: RunConfig{MaxTurns: 5},
	})
	if outcome.Status != subAgentStatusFailed {
		t.Fatalf("outcome.Status = %q, want failed", outcome.Status)
	}
	if !strings.Contains(outcome.ErrMsg, "Partial progress before the run ended:") ||
		!strings.Contains(outcome.ErrMsg, "researched 12 sources") {
		t.Fatalf("ErrMsg lost the partial findings: %q", outcome.ErrMsg)
	}
}
