package agentsdk

import (
	"encoding/json"
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

func TestAutoTrackerIgnoresDiverseExploration(t *testing.T) {
	t.Parallel()

	// Reading different files and alternating grep/read_file on distinct targets
	// must not be mistaken for a stuck loop, even though the tool *names* repeat.
	var tracker AutoTracker
	calls := []struct{ name, input string }{
		{"grep", `{"pattern":"funcA"}`},
		{"read_file", `{"path":"a.go"}`},
		{"read_file", `{"path":"b.go"}`},
		{"grep", `{"pattern":"funcB"}`},
		{"read_file", `{"path":"c.go"}`},
		{"grep", `{"pattern":"funcC"}`},
		{"read_file", `{"path":"d.go"}`},
		{"read_file", `{"path":"e.go"}`},
		{"grep", `{"pattern":"funcD"}`},
		{"read_file", `{"path":"f.go"}`},
	}
	for _, c := range calls {
		tracker.Update([]RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: c.name, Input: json.RawMessage(c.input)}}})
		if cb := tracker.CheckCircuitBreakers(); cb.Tripped {
			t.Fatalf("circuit breaker tripped on diverse exploration: %s", cb.Reason)
		}
	}
	if nudge := BuildSmartNudge(&tracker, "plan"); strings.Contains(nudge, "stuck in a loop") {
		t.Fatalf("nudge = %q, should not warn about a loop during diverse exploration", nudge)
	}
}

func TestAutoTrackerDetectsIdenticalCallLoop(t *testing.T) {
	t.Parallel()

	// The same tool with the same input repeated is a genuine no-progress loop.
	var tracker AutoTracker
	for i := 0; i < 6; i++ {
		tracker.Update([]RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "read_file", Input: json.RawMessage(`{"path":"same.go"}`)}}})
	}
	cb := tracker.CheckCircuitBreakers()
	if !cb.Tripped || !strings.Contains(cb.Reason, "tool loop") {
		t.Fatalf("circuit breaker = %+v, want tool-loop breaker for identical repeated calls", cb)
	}
}
