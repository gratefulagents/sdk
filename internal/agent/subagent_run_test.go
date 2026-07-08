package agent

import "testing"

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
