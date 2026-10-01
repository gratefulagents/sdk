package agentsdk

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAutoTrackerDetectsNoToolStall(t *testing.T) {
	t.Parallel()

	var tracker AutoTracker
	// Reasoning models may legitimately plan without tools for a few turns;
	// the breaker must stay quiet below the threshold and trip at it.
	for i := 0; i < defaultCBMaxNoToolTurns-1; i++ {
		tracker.Update([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "thinking"}}})
		if cb := tracker.CheckCircuitBreakers(); cb.Tripped {
			t.Fatalf("circuit breaker tripped after %d no-tool turns: %+v", i+1, cb)
		}
	}
	tracker.Update([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "thinking"}}})
	cb := tracker.CheckCircuitBreakers()
	if !cb.Tripped || !strings.Contains(cb.Reason, "no tool calls") {
		t.Fatalf("circuit breaker = %+v, want no-tool stall", cb)
	}
}

func TestBuildSmartNudgeWarnsOnRepeatedNoToolTurns(t *testing.T) {
	t.Parallel()

	var tracker AutoTracker
	for i := 0; i < cbNoToolWarningTurns; i++ {
		tracker.Update([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done soon"}}})
	}

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

func TestAutoTrackerSameErrorStreakResetsAfterSuccessfulTurn(t *testing.T) {
	failing := func(id string) []RunItem {
		return []RunItem{
			{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: id, Name: "bash", Input: []byte(`{"cmd":"` + id + `"}`)}},
			{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: id, Content: "timed out after 120s", IsError: true}},
		}
	}
	succeeding := func(id string) []RunItem {
		return []RunItem{
			{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: id, Name: "read", Input: []byte(`{"path":"` + id + `"}`)}},
			{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: id, Content: "contents"}},
		}
	}
	tracker := &AutoTracker{}
	for i := 0; i < defaultCBMaxSameErrors*2; i++ {
		tracker.Update(failing(fmt.Sprintf("fail_%d", i)))
		tracker.Update(succeeding(fmt.Sprintf("ok_%d", i)))
	}
	if result := tracker.CheckCircuitBreakers(); result.Tripped {
		t.Fatalf("breaker tripped on errors separated by successful work: %s", result.Reason)
	}
	if nudge := BuildSmartNudge(tracker, ""); strings.Contains(nudge, "same error") {
		t.Fatalf("nudge misfired: %s", nudge)
	}

	for i := 0; i < defaultCBMaxSameErrors; i++ {
		tracker.Update(failing(fmt.Sprintf("stuck_%d", i)))
	}
	if result := tracker.CheckCircuitBreakers(); !result.Tripped || !strings.Contains(result.Reason, "same error") {
		t.Fatalf("consecutive identical errors did not trip the breaker: %+v", result)
	}
}
