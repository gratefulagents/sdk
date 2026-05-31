package agentsdk

import (
	"strings"
	"testing"
)

func TestAutoTrackerDetectsNoToolStall(t *testing.T) {
	t.Parallel()

	var tracker AutoTracker
	for i := 0; i < 3; i++ {
		tracker.Update([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "thinking"}}})
	}
	cb := tracker.CheckCircuitBreakers()
	if !cb.Tripped || !strings.Contains(cb.Reason, "no tool calls") {
		t.Fatalf("circuit breaker = %+v, want no-tool stall", cb)
	}
}

func TestBuildSmartNudgeWarnsOnRepeatedNoToolTurns(t *testing.T) {
	t.Parallel()

	var tracker AutoTracker
	tracker.Update([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done soon"}}})
	tracker.Update([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "still done soon"}}})

	nudge := BuildSmartNudge(&tracker, "shipping")
	if !strings.Contains(nudge, "WARNING") || !strings.Contains(nudge, "tool calls") {
		t.Fatalf("nudge = %q, want warning", nudge)
	}
	for _, want := range []string{
		"Finish is a hard stop",
		"not a progress report",
		"backlog item",
		"call a tool for the top item",
	} {
		if !strings.Contains(nudge, want) {
			t.Fatalf("nudge = %q, want %q", nudge, want)
		}
	}
}

func TestAutoTrackerDetectsRepeatedToolCycle(t *testing.T) {
	t.Parallel()

	var tracker AutoTracker
	for i := 0; i < 3; i++ {
		tracker.Update([]RunItem{
			{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "grep"}},
			{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "bash"}},
		})
	}
	cb := tracker.CheckCircuitBreakers()
	if !cb.Tripped || !strings.Contains(cb.Reason, "tool loop") {
		t.Fatalf("circuit breaker = %+v, want tool-loop breaker", cb)
	}
}
