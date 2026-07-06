package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// Subagent usage must roll up into the run-level tracker as it happens:
// cost, input/output tokens, AND cache tokens, through arbitrarily nested
// child trackers. This is what keeps cost-per-run / tokens-per-run metrics
// (and cost-cap enforcement) accurate while subagents are still running.
func TestChildTrackerForwardsLLMUsageToAncestors(t *testing.T) {
	parent := NewProgressTracker()
	parent.SetSession(1, "implementing")
	child := NewChildTracker(parent, "task_child")
	grandchild := NewChildTracker(child, "task_grandchild")

	childUsage := Usage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 2000, CacheCreateTokens: 300}
	grandchildUsage := Usage{InputTokens: 10, OutputTokens: 4, CacheReadTokens: 500, CacheCreateTokens: 50}

	child.RecordLLMUsage("model-a", 0.25, childUsage)
	grandchild.RecordLLMUsage("model-b", 0.05, grandchildUsage)

	parentSnap := parent.Snapshot()
	if math.Abs(parentSnap.CostUsd-0.30) > 1e-9 {
		t.Errorf("parent CostUsd = %v, want 0.30", parentSnap.CostUsd)
	}
	if parentSnap.InputTokens != 110 || parentSnap.OutputTokens != 44 {
		t.Errorf("parent tokens = %d/%d, want 110/44", parentSnap.InputTokens, parentSnap.OutputTokens)
	}
	if parentSnap.CacheReadInputTokens != 2500 || parentSnap.CacheCreationInputTokens != 350 {
		t.Errorf("parent cache tokens = %d/%d, want 2500/350",
			parentSnap.CacheReadInputTokens, parentSnap.CacheCreationInputTokens)
	}
	if mu, ok := parentSnap.ModelUsage["model-b"]; !ok || mu.InputTokens != 10 {
		t.Errorf("parent ModelUsage[model-b] = %+v (ok=%v), want grandchild usage forwarded", mu, ok)
	}

	// The intermediate child sees its own usage plus the grandchild's,
	// but not vice versa.
	childSnap := child.Snapshot()
	if childSnap.InputTokens != 110 {
		t.Errorf("child InputTokens = %d, want 110", childSnap.InputTokens)
	}
	grandchildSnap := grandchild.Snapshot()
	if grandchildSnap.InputTokens != 10 {
		t.Errorf("grandchild InputTokens = %d, want 10", grandchildSnap.InputTokens)
	}
}

// Tool calls made by subagents count toward the run's tool-call total, but
// must not disturb the parent's own activity/step-inference state.
func TestChildTrackerForwardsToolCallCounts(t *testing.T) {
	parent := NewProgressTracker()
	parent.SetSession(1, "implementing")
	child := NewChildTracker(parent, "task_child")
	grandchild := NewChildTracker(child, "task_grandchild")

	child.RecordToolUse("Edit", "/file.go", "tu_1", 1, `{}`, "", "")
	grandchild.RecordToolUse("Bash", "ls", "tu_2", 1, `{}`, "", "")

	parentSnap := parent.Snapshot()
	if parentSnap.ToolCallCount != 2 {
		t.Errorf("parent ToolCallCount = %d, want 2", parentSnap.ToolCallCount)
	}
	if parentSnap.CurrentStep != "implementing" {
		t.Errorf("parent CurrentStep = %q, want unchanged %q", parentSnap.CurrentStep, "implementing")
	}
	if parentSnap.LastActivity != "" {
		t.Errorf("parent LastActivity = %q, want untouched by forwarded tool calls", parentSnap.LastActivity)
	}
	if child.Snapshot().ToolCallCount != 2 {
		t.Errorf("child ToolCallCount = %d, want 2", child.Snapshot().ToolCallCount)
	}
}

// With live per-call forwarding, RecordSubagentCompleted must not re-add the
// subagent's usage: that would double count every completed subagent.
func TestRecordSubagentCompletedDoesNotDoubleCountForwardedUsage(t *testing.T) {
	parent := NewProgressTracker()
	parent.SetSession(1, "implementing")
	child := NewChildTracker(parent, "task_1")

	usage := Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 8000, CacheCreateTokens: 200}
	child.RecordLLMUsage("model-a", 0.10, usage)

	parent.RecordSubagentStarted("task_1", "tu_1", "explore", "explore", "model-a", "", "prompt")
	parent.RecordSubagentCompleted("task_1", "completed", "done", 0.10, 3, usage, "end_turn", nil, nil)

	snap := parent.Snapshot()
	if math.Abs(snap.CostUsd-0.10) > 1e-9 {
		t.Errorf("parent CostUsd = %v, want 0.10 (no double count)", snap.CostUsd)
	}
	if snap.InputTokens != 1000 || snap.OutputTokens != 500 {
		t.Errorf("parent tokens = %d/%d, want 1000/500 (no double count)", snap.InputTokens, snap.OutputTokens)
	}
	if snap.CacheReadInputTokens != 8000 || snap.CacheCreationInputTokens != 200 {
		t.Errorf("parent cache tokens = %d/%d, want 8000/200", snap.CacheReadInputTokens, snap.CacheCreationInputTokens)
	}
}

// A subagent that fails mid-run must still contribute everything it consumed
// before failing: to the run totals (via live forwarding) and to its own
// completion outcome/event (recovered from the child tracker, since
// Runner.Run returns a nil result on error).
func TestRunSubAgentOnceFailureKeepsPartialUsage(t *testing.T) {
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			return "echoed: " + string(input), nil
		},
	}
	model := &mockModel{
		costPerCall: 0.25,
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "call1", Name: "echo", Input: json.RawMessage(`"hello"`),
					}},
				},
				Usage: Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, CacheCreateTokens: 10},
			},
		},
		errors: []error{nil, errors.New("model exploded")},
	}
	runner := NewRunnerWithModel(model)
	parentTracker := NewProgressTracker()
	parentTracker.SetSession(1, "implementing")
	var buf strings.Builder
	es := NewEventStream(&buf)

	outcome := runSubAgentOnce(context.Background(), subAgentRunSpec{
		Runner:      runner,
		Agent:       &Agent{Name: "explore", Tools: []Tool{echoTool}},
		Message:     "do the thing",
		TaskID:      "task_fail",
		Tracker:     parentTracker,
		EventStream: es,
		RunConfig:   RunConfig{MaxTurns: 5},
	})

	if outcome.Status != subAgentStatusFailed {
		t.Fatalf("outcome.Status = %q, want failed", outcome.Status)
	}
	if outcome.Tokens != 120 || outcome.ToolCount != 1 {
		t.Errorf("outcome tokens/tools = %d/%d, want 120/1", outcome.Tokens, outcome.ToolCount)
	}
	if math.Abs(outcome.CostUSD-0.25) > 1e-9 || !outcome.CostKnown {
		t.Errorf("outcome cost = %v (known=%v), want 0.25 (known=true)", outcome.CostUSD, outcome.CostKnown)
	}
	if outcome.Usage.CacheReadTokens != 50 || outcome.Usage.CacheCreateTokens != 10 {
		t.Errorf("outcome cache tokens = %d/%d, want 50/10",
			outcome.Usage.CacheReadTokens, outcome.Usage.CacheCreateTokens)
	}

	// Run totals include the failed subagent's spend.
	snap := parentTracker.Snapshot()
	if math.Abs(snap.CostUsd-0.25) > 1e-9 {
		t.Errorf("parent CostUsd = %v, want 0.25", snap.CostUsd)
	}
	if snap.InputTokens != 100 || snap.OutputTokens != 20 {
		t.Errorf("parent tokens = %d/%d, want 100/20", snap.InputTokens, snap.OutputTokens)
	}
	if snap.CacheReadInputTokens != 50 || snap.CacheCreationInputTokens != 10 {
		t.Errorf("parent cache tokens = %d/%d, want 50/10", snap.CacheReadInputTokens, snap.CacheCreationInputTokens)
	}
	if snap.ToolCallCount != 1 {
		t.Errorf("parent ToolCallCount = %d, want 1", snap.ToolCallCount)
	}

	// The completion event reports the partial usage instead of zeros.
	var completed *ContentEvent
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev ContentEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		if ev.Type == "subagent_status" && ev.Status == "failed" {
			completed = &ev
			break
		}
	}
	if completed == nil {
		t.Fatal("no failed subagent_status event emitted")
	}
	if completed.SubagentTokens != 120 || completed.SubagentToolCount != 1 {
		t.Errorf("event tokens/tools = %d/%d, want 120/1", completed.SubagentTokens, completed.SubagentToolCount)
	}
	if math.Abs(completed.SubagentCostUsd-0.25) > 1e-9 || !completed.SubagentCostKnown {
		t.Errorf("event cost = %v (known=%v), want 0.25 (known=true)", completed.SubagentCostUsd, completed.SubagentCostKnown)
	}
}
