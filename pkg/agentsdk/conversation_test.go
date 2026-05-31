package agentsdk

import (
	"strings"
	"testing"
)

func TestBuildConversationTailRespectsHistoryFloorAndCurrentMessage(t *testing.T) {
	t.Parallel()

	messages := []ConversationMessage{
		{ID: 1, Role: "user", Content: "old task"},
		{ID: 2, Role: "assistant", Content: "old summary"},
		{ID: 3, Role: "user", Content: "current user message"},
		{ID: 4, Role: "system", Content: "[SYSTEM] continue with shipping"},
		{ID: 5, Role: "assistant", Content: "recent assistant summary"},
	}

	items := BuildConversationTail(messages, WorkingState{HistoryFloorMessageID: 2}, 3, 8)
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].Agent == nil || items[0].Agent.Name != "system-summary" {
		t.Fatalf("items[0].Agent = %#v, want system-summary", items[0].Agent)
	}
	if got := items[0].Message.Text; got != "[SYSTEM] continue with shipping" {
		t.Fatalf("items[0].Message.Text = %q", got)
	}
	if items[1].Agent == nil || items[1].Agent.Name != "assistant-summary" {
		t.Fatalf("items[1].Agent = %#v, want assistant-summary", items[1].Agent)
	}
}

func TestBuildWorkingStateContextUsesMostRecentProgress(t *testing.T) {
	t.Parallel()

	state := WorkingState{
		Goal:                 "finish github auth end to end",
		CurrentMode:          "deep",
		CurrentPhase:         "shipping",
		LastUserMessage:      "also wire github app callbacks",
		LastAssistantSummary: "implemented oauth callback plumbing",
		RecentTurnSummaries:  []string{"summary-1", "summary-2", "summary-3", "summary-4", "summary-5"},
	}

	context := BuildWorkingStateContext(state)
	if !strings.Contains(context, "Current objective: finish github auth end to end") {
		t.Fatalf("context = %q, want objective", context)
	}
	if !strings.Contains(context, "Mode: deep (phase: shipping)") {
		t.Fatalf("context = %q, want mode/phase", context)
	}
	if strings.Contains(context, "summary-1") {
		t.Fatalf("context = %q, want only last four summaries", context)
	}
	if !strings.Contains(context, "summary-5") {
		t.Fatalf("context = %q, want newest summary", context)
	}
}

func TestBuildAssistantTurnSummaryCapturesToolsAndIssues(t *testing.T) {
	t.Parallel()

	items := []RunItem{
		{
			Type:    RunItemMessage,
			Agent:   &Agent{Name: "assistant"},
			Message: &MessageOutput{Text: "Investigated the auth wiring and found the missing callback registration."},
		},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "grep"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "bash"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "bash"}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "1", IsError: true, Content: "oauth token missing"}},
	}

	summary := BuildAssistantTurnSummary(items)
	if !strings.Contains(summary, "Investigated the auth wiring") {
		t.Fatalf("summary = %q, want assistant text", summary)
	}
	if !strings.Contains(summary, "bash x 2") || !strings.Contains(summary, "grep x 1") {
		t.Fatalf("summary = %q, want tool counts", summary)
	}
	if !strings.Contains(summary, "Issues: oauth token missing") {
		t.Fatalf("summary = %q, want error summary", summary)
	}
}

func TestSelectNextUserMessagePrioritizesImmediateOverEarlierQueued(t *testing.T) {
	t.Parallel()

	messages := []UserMessage{
		{ID: 30, Content: "queued next", Mode: UserMessageModeEnqueue},
		{ID: 31, Content: "steer now", Mode: UserMessageModeImmediate},
	}

	msg, ok, skipCursor, immediate := SelectNextUserMessage(messages, map[int64]struct{}{})
	if !ok {
		t.Fatal("expected pending message")
	}
	if msg.ID != 31 {
		t.Fatalf("message ID = %d, want immediate message 31", msg.ID)
	}
	if skipCursor != 0 {
		t.Fatalf("skipCursor = %d, want 0 because queued message must remain pending", skipCursor)
	}
	if !immediate {
		t.Fatal("expected immediate message to win")
	}
}

func TestCollectImmediateRunItemsPreservesOrderAndCursor(t *testing.T) {
	t.Parallel()

	messages := []UserMessage{
		{ID: 20, Content: "queued", Mode: UserMessageModeEnqueue},
		{ID: 21, Content: "first immediate", Mode: UserMessageModeImmediate},
		{ID: 22, Content: "second immediate", Mode: UserMessageModeImmediate},
	}
	consumed := map[int64]struct{}{}

	items, cursor := CollectImmediateRunItems(messages, consumed)
	if cursor != 22 {
		t.Fatalf("cursor = %d, want 22", cursor)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].Message == nil || items[0].Message.Text != "first immediate" {
		t.Fatalf("first item = %#v, want first immediate", items[0].Message)
	}
	if _, ok := consumed[21]; !ok {
		t.Fatal("expected message 21 to be marked consumed")
	}
}
