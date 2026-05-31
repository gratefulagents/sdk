package agent

import (
	"strings"
	"testing"
)

func TestMaybeCompactRunItemsPreservesOriginalTaskAndAddsSummary(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "Original task: investigate the flaky integration test"}},
	}
	for i := 0; i < 18; i++ {
		items = append(items,
			RunItem{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: strings.Repeat("assistant context ", 10)}},
			RunItem{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "grep"}},
			RunItem{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{Content: strings.Repeat("tool output ", 10)}},
		)
	}

	compacted, before, after, ok, _ := MaybeCompactRunItems(items, CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               200,
		TargetTokens:                100,
		PreserveRecentItems:         4,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          3,
	})
	if !ok {
		t.Fatal("expected compaction to run")
	}
	if after >= before {
		t.Fatalf("tokens after = %d, want less than before %d", after, before)
	}
	gotText := Items.ExtractText(compacted)
	if !strings.Contains(gotText, "Original task: investigate the flaky integration test") {
		t.Fatalf("compacted text = %q, want original task preserved", gotText)
	}
	if !strings.Contains(gotText, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("compacted text = %q, want compacted history summary", gotText)
	}
}

func TestSummarizeCompactedHistoryBuildsReadableHandoff(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "continue compaction work; next implement tests"}},
		{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: "Updated internal/agent/history_compaction.go and still need to run tests."}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "Bash", Input: []byte(`{"command":"sed -n '250,320p' cmd/agent/plan.go"}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{Content: ".dockerignore .git/HEAD .git/config .git/description cmd/agent/plan.go internal/auth/session.go\nPASS internal/agent"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "Bash", Input: []byte(`{"command":"go test ./internal/agent"}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{Content: "FAIL\tinternal/agent\nhistory_compaction_test.go: after=107 want <=90", IsError: true}},
	}

	summary := summarizeCompactedHistory(items, 3)
	for _, want := range []string{
		"Conversation summary:",
		"Scope: 6 earlier messages compacted",
		"Tools mentioned: Bash.",
		"Recent user requests:",
		"Pending work:",
		"Key files referenced:",
		"cmd/agent/plan.go",
		"Key timeline:",
		"assistant: tool_use Bash",
		"tool: tool_result:",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary = %q, want %q", summary, want)
		}
	}
	for _, unwanted := range []string{"Tool activity:", "Bash ran 2 times", "Issues and errors:", "Notable outputs:", "Current work: FAIL"} {
		if strings.Contains(summary, unwanted) {
			t.Fatalf("summary = %q, did not want non-Claw summary section %q", summary, unwanted)
		}
	}
}

func TestSummarizeCompactedHistoryMergesPriorSummaryLikeClaw(t *testing.T) {
	items := []RunItem{
		{
			Type:  RunItemMessage,
			Agent: &Agent{Name: "context-summary"},
			Message: &MessageOutput{Text: `[COMPACTED HISTORY SUMMARY]
Conversation summary:
- Scope: 56 earlier messages compacted (user=0, assistant=31, tool=25).
- Tools mentioned: Bash.
- Key timeline:
  - assistant: old command output that should not be repeated as a highlight`},
		},
		{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: "Next: finish the compaction merge behavior."}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "Bash", Input: []byte(`{"command":"go test ./internal/agent"}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{Content: "ok\tgithub.com/gratefulagents/sdk/internal/agent"}},
	}

	summary := summarizeCompactedHistory(items, 3)
	for _, want := range []string{
		"Previously compacted context:",
		"Newly compacted context:",
		"Scope: 56 earlier messages compacted",
		"Scope: 3 earlier messages compacted",
		"Key timeline:",
		"assistant: tool_use Bash",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary = %q, want %q", summary, want)
		}
	}
	for _, unwanted := range []string{
		"Current work: [COMPACTED HISTORY SUMMARY]",
		"old command output that should not be repeated as a highlight",
		"Tool activity:",
		"Notable outputs:",
	} {
		if strings.Contains(summary, unwanted) {
			t.Fatalf("summary = %q, did not want %q", summary, unwanted)
		}
	}
}

func TestMaybeCompactRunItemsNoopBelowThreshold(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "Short task"}},
		{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: "Short answer"}},
	}

	compacted, before, after, ok, _ := MaybeCompactRunItems(items, CompactionConfig{
		Enabled:       true,
		TriggerTokens: 1000,
	})
	if ok {
		t.Fatal("expected no compaction below threshold")
	}
	if before != after {
		t.Fatalf("before/after = %d/%d, want equal", before, after)
	}
	if len(compacted) != len(items) {
		t.Fatalf("compacted len = %d, want %d", len(compacted), len(items))
	}
}

func TestMaybeCompactRunItemsForRequestCountsOverhead(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "Original task: diagnose context overflow"}},
	}
	for i := 0; i < 12; i++ {
		items = append(items, RunItem{
			Type:    RunItemMessage,
			Agent:   &Agent{Name: "assistant"},
			Message: &MessageOutput{Text: strings.Repeat("older request context ", 8)},
		})
	}

	beforeItems := estimateRunItemsTokens(items)
	cfg := CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               beforeItems + 10,
		TargetTokens:                120,
		PreserveRecentItems:         1,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          2,
	}
	compacted, before, after, ok, _ := MaybeCompactRunItemsForRequest(items, cfg, 80)
	if !ok {
		t.Fatal("expected request overhead to trigger compaction")
	}
	if before <= cfg.TriggerTokens {
		t.Fatalf("before = %d, want above trigger %d once overhead is counted", before, cfg.TriggerTokens)
	}
	if after >= before {
		t.Fatalf("after = %d, want below before %d", after, before)
	}
	if gotText := Items.ExtractText(compacted); !strings.Contains(gotText, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("compacted text = %q, want summary marker", gotText)
	}
}

func TestMaybeCompactRunItemsPreservesToolPairs(t *testing.T) {
	// Reproduce the bug: a tool_output in the tail references a tool_call
	// that was in the compacted middle section. Without the pair-integrity
	// fix, the API rejects the orphaned tool_output.
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "Original task: fix the bug"}},
	}
	// Fill middle with enough items to trigger compaction. Each triplet
	// is: assistant message, tool_call (with ID), tool_output (with CallID).
	for i := 0; i < 20; i++ {
		callID := "call_" + strings.Repeat("x", i+1)
		items = append(items,
			RunItem{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: strings.Repeat("reasoning ", 10)}},
			RunItem{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: callID, Name: "Grep", Input: []byte(`{}`)}},
			RunItem{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: callID, Content: strings.Repeat("output ", 10)}},
		)
	}

	// Compaction preserves only 4 recent items. The last 4 items are:
	// [assistant, tool_call(id=call_xxx..x20), tool_output(call_xxx..x20), ...]
	// If PreserveRecentItems=2, the tail may include a tool_output whose
	// tool_call is in the compacted middle.
	compacted, _, _, ok, _ := MaybeCompactRunItems(items, CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               100,
		TargetTokens:                50,
		PreserveRecentItems:         3,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          3,
	})
	if !ok {
		t.Fatal("expected compaction to trigger")
	}

	// Verify: every tool_output in the compacted result has a matching tool_call.
	callIDs := map[string]bool{}
	for _, item := range compacted {
		if item.Type == RunItemToolCall && item.ToolCall != nil {
			callIDs[item.ToolCall.ID] = true
		}
	}
	for _, item := range compacted {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID != "" {
			if !callIDs[item.ToolOutput.CallID] {
				t.Fatalf("orphaned tool_output with CallID=%q: no matching tool_call in compacted result", item.ToolOutput.CallID)
			}
		}
	}

	// Verify: every tool_call in the compacted result has a matching tool_output.
	outputCallIDs := map[string]bool{}
	for _, item := range compacted {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil {
			outputCallIDs[item.ToolOutput.CallID] = true
		}
	}
	for _, item := range compacted {
		if item.Type == RunItemToolCall && item.ToolCall != nil && item.ToolCall.ID != "" {
			if !outputCallIDs[item.ToolCall.ID] {
				t.Fatalf("orphaned tool_call with ID=%q: no matching tool_output in compacted result", item.ToolCall.ID)
			}
		}
	}
}

func TestMaybeCompactRunItemsReducesFurtherToMeetTargetTokens(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "Original task: finish the auth migration"}},
	}
	for i := 0; i < 14; i++ {
		items = append(items, RunItem{
			Type:    RunItemMessage,
			Agent:   &Agent{Name: "assistant"},
			Message: &MessageOutput{Text: strings.Repeat("recent context ", 6)},
		})
	}

	compacted, before, after, ok, _ := MaybeCompactRunItems(items, CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               40,
		TargetTokens:                90,
		PreserveRecentItems:         10,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          2,
	})
	if !ok {
		t.Fatal("expected compaction to trigger")
	}
	if after > 90 {
		t.Fatalf("after = %d, want <= 90 (before=%d, text=%q)", after, before, Items.ExtractText(compacted))
	}
}
