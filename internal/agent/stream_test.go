package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewProgressTracker(t *testing.T) {
	tracker := NewProgressTracker()

	snap := tracker.Snapshot()
	if snap.CurrentStep != "" {
		t.Errorf("CurrentStep = %q, want empty", snap.CurrentStep)
	}
	if snap.SessionNumber != 0 {
		t.Errorf("SessionNumber = %d, want 0", snap.SessionNumber)
	}
}

func TestProgressTracker_SetSession(t *testing.T) {
	tracker := NewProgressTracker()

	tracker.SetSession(1, "planning")

	snap := tracker.Snapshot()
	if snap.SessionNumber != 1 {
		t.Errorf("SessionNumber = %d, want 1", snap.SessionNumber)
	}
	if snap.CurrentStep != "planning" {
		t.Errorf("CurrentStep = %q, want %q", snap.CurrentStep, "planning")
	}
}

func TestProgressTracker_RecordToolUse(t *testing.T) {
	tracker := NewProgressTracker()
	tracker.SetSession(1, "implementing")

	tracker.RecordToolUse("Bash", "echo hello", "tu_1", 3, `{"command":"echo hello"}`, "", "")

	snap := tracker.Snapshot()
	if snap.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", snap.ToolCallCount)
	}
}

func TestProgressTracker_RecordSystemInitIncludesPermissionMode(t *testing.T) {
	tracker := NewProgressTracker()

	tracker.RecordSystemInit("gpt-5.4", "/repo", []string{"Bash", "Read"}, 25, []string{"github"}, "workspace-write")

	snap := tracker.Snapshot()
	if snap.CurrentStep != "starting" {
		t.Fatalf("CurrentStep = %q, want starting", snap.CurrentStep)
	}
}

func TestProgressTracker_RecordLifecycleEvent(t *testing.T) {
	tracker := NewProgressTracker()
	tracker.RecordLifecycleEvent("session-end", "done")
	snap := tracker.Snapshot()
	if len(snap.Events) == 0 || snap.Events[0].EventType != "session-end" {
		t.Fatalf("Events = %#v, want lifecycle event", snap.Events)
	}
}

func TestProgressTracker_RecordToolResult(t *testing.T) {
	tracker := NewProgressTracker()
	tracker.RecordToolResult("tu_1", "Bash", "hello\n", false, 150, "", "")
	snap := tracker.Snapshot()
	if len(snap.Events) == 0 || snap.Events[0].EventType != "tool_result" {
		t.Fatalf("Events = %#v, want tool result event", snap.Events)
	}
}

func TestProgressTracker_RecordAssistantText(t *testing.T) {
	tracker := NewProgressTracker()

	tracker.RecordAssistantText("Hello, I'll help you.")

	if tracker.LastAssistantText() != "Hello, I'll help you." {
		t.Errorf("LastAssistantText = %q", tracker.LastAssistantText())
	}
}

func TestProgressTracker_RecordSessionComplete(t *testing.T) {
	tracker := NewProgressTracker()
	tracker.SetSession(1, "implementing")

	usage := Usage{
		InputTokens:       10000,
		OutputTokens:      5000,
		CacheReadTokens:   2000,
		CacheCreateTokens: 500,
	}

	// RecordLLMUsage accumulates tokens/cost; RecordSessionComplete emits the event.
	tracker.RecordLLMUsage("claude-sonnet-4-6", 0.05, Usage{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CacheReadTokens:   usage.CacheReadTokens,
		CacheCreateTokens: usage.CacheCreateTokens,
	})
	tracker.RecordSessionComplete("claude-sonnet-4-6", 0.05, 10, usage, 30_000_000_000, "end_turn")

	snap := tracker.Snapshot()
	if snap.InputTokens != 10000 {
		t.Errorf("InputTokens = %d, want 10000", snap.InputTokens)
	}
	if snap.OutputTokens != 5000 {
		t.Errorf("OutputTokens = %d, want 5000", snap.OutputTokens)
	}

	// Check model usage tracking.
	mu, ok := snap.ModelUsage["claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing model usage for claude-sonnet-4-6")
	}
	if mu.InputTokens != 10000 {
		t.Errorf("ModelUsage.InputTokens = %d, want 10000", mu.InputTokens)
	}
}

func TestProgressTracker_RecordCompactBoundary(t *testing.T) {
	tracker := NewProgressTracker()
	// RecordCompactBoundary emits an OTel span but should not panic without a processor.
	tracker.RecordCompactBoundary(50000, 20000, "")
}

func TestProgressTracker_RecordSubagentLifecycle(t *testing.T) {
	tracker := NewProgressTracker()
	tracker.SetSession(1, "implementing")

	tracker.RecordSubagentStarted("task_1", "tu_agent_1", "Search codebase", "Explore", "claude-sonnet-4-6", "", "find all API endpoints")

	snap := tracker.Snapshot()
	if snap.AgentCount != 1 {
		t.Errorf("AgentCount = %d, want 1", snap.AgentCount)
	}

	// Record completion.
	usage := Usage{InputTokens: 1000, OutputTokens: 500}
	tracker.RecordSubagentCompleted("task_1", "completed", "Found 5 endpoints", 0.01, 3, usage, "end_turn", nil, nil)
}

func TestNewChildTracker(t *testing.T) {
	parent := NewProgressTracker()
	parent.SetSession(1, "implementing")

	child := NewChildTracker(parent, "task_42")

	child.RecordToolUse("Read", "/some/file.go", "tu_child_1", 1, `{"file_path":"/some/file.go"}`, "", "")
	// No panic = pass.
}

func TestProgressTracker_EventRingBuffer(t *testing.T) {
	tracker := NewProgressTracker()

	// Generate more events than MaxEvents.
	for i := 0; i < MaxEvents+10; i++ {
		tracker.SetStep("step_" + string(rune('a'+i%26)))
	}

	snap := tracker.Snapshot()
	if len(snap.Events) > MaxEvents {
		t.Errorf("Events = %d, want <= %d", len(snap.Events), MaxEvents)
	}
}

func TestProgressTracker_InferStep(t *testing.T) {
	tracker := NewProgressTracker()
	tracker.SetSession(1, "starting")

	// LSP tool should transition to "exploring".
	tracker.RecordToolUse("LSP", "hover", "tu_1", 1, `{}`, "", "")
	snap := tracker.Snapshot()
	if snap.CurrentStep != "exploring" {
		t.Errorf("CurrentStep after LSP = %q, want %q", snap.CurrentStep, "exploring")
	}

	// Edit tool should transition to "implementing".
	tracker.RecordToolUse("Edit", "/file.go", "tu_2", 2, `{}`, "", "")
	snap = tracker.Snapshot()
	if snap.CurrentStep != "implementing" {
		t.Errorf("CurrentStep after Edit = %q, want %q", snap.CurrentStep, "implementing")
	}

	// git commit should transition to "committing".
	tracker.RecordToolUse("Bash", "git commit -m 'fix'", "tu_3", 3, `{"command":"git commit -m 'fix'"}`, "", "")
	snap = tracker.Snapshot()
	if snap.CurrentStep != "committing" {
		t.Errorf("CurrentStep after git commit = %q, want %q", snap.CurrentStep, "committing")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"short", 10, "short"},
		{"hello world", 8, "hello..."},
		{"", 5, ""},
		{"with\nnewlines", 20, "with newlines"},
		{"  padded  ", 20, "padded"},
	}
	for _, tt := range tests {
		got := Truncate(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestTruncateBytes(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"short", 10, "short"},
		{"hello world", 8, "hello..."},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := TruncateBytes(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("TruncateBytes(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestExtractHelpers(t *testing.T) {
	t.Run("ExtractFilePath", func(t *testing.T) {
		input := json.RawMessage(`{"file_path":"/foo/bar.go"}`)
		if got := ExtractFilePath(input); got != "/foo/bar.go" {
			t.Errorf("got %q, want %q", got, "/foo/bar.go")
		}
	})

	t.Run("ExtractGrepPattern", func(t *testing.T) {
		input := json.RawMessage(`{"pattern":"func\\s+main"}`)
		if got := ExtractGrepPattern(input); got != `func\s+main` {
			t.Errorf("got %q, want %q", got, `func\s+main`)
		}
	})

	t.Run("ExtractBashCommand", func(t *testing.T) {
		input := json.RawMessage(`{"command":"go build ./..."}`)
		if got := ExtractBashCommand(input); got != "go build ./..." {
			t.Errorf("got %q, want %q", got, "go build ./...")
		}
	})

	t.Run("ExtractAgentMeta", func(t *testing.T) {
		input := json.RawMessage(`{"description":"Find APIs","prompt":"search for endpoints","subagent_type":"Explore","model":"medium"}`)
		meta := ExtractAgentMeta(input)
		if meta.Description != "Find APIs" {
			t.Errorf("Description = %q, want %q", meta.Description, "Find APIs")
		}
		if meta.SubagentType != "Explore" {
			t.Errorf("SubagentType = %q, want %q", meta.SubagentType, "Explore")
		}
	})
}

func TestProgressTracker_DrainPendingEvents(t *testing.T) {
	tracker := NewProgressTracker()
	tracker.SetSession(1, "test")

	events := tracker.DrainPendingEvents()
	if len(events) == 0 {
		t.Fatal("expected pending events from SetSession")
	}

	// Second drain should be empty.
	events2 := tracker.DrainPendingEvents()
	if len(events2) != 0 {
		t.Errorf("expected no pending events after drain, got %d", len(events2))
	}
}

func TestTruncateMiddle(t *testing.T) {
	short := "short text"
	if got := TruncateMiddle(short, 100); got != short {
		t.Errorf("short input should be unchanged, got %q", got)
	}

	long := strings.Repeat("a", 500) + "MIDDLE" + strings.Repeat("z", 500)
	got := TruncateMiddle(long, 200)
	if len([]rune(got)) > 230 { // marker slack
		t.Errorf("truncated output too long: %d", len(got))
	}
	if !strings.HasPrefix(got, "aaa") {
		t.Errorf("head not preserved: %q", got[:20])
	}
	if !strings.HasSuffix(got, "zzz") {
		t.Errorf("tail not preserved: %q", got[len(got)-20:])
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("missing elision marker: %q", got)
	}

	// n <= 0 means no truncation.
	if got := TruncateMiddle(long, 0); got != long {
		t.Error("n=0 should disable truncation")
	}
}
