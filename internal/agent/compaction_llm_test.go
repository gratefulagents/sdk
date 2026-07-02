package agent

import (
	"context"
	"strings"
	"testing"
)

func manyCompactableItems(n int) []RunItem {
	items := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "refactor the composer window"}}}
	for i := 0; i < n; i++ {
		items = append(items,
			RunItem{Type: RunItemReasoning, Agent: &Agent{Name: "a"}, Reasoning: &ReasoningData{Text: strings.Repeat("design thought ", 40)}},
			RunItem{Type: RunItemToolCall, Agent: &Agent{Name: "a"}, ToolCall: &ToolCallData{ID: callID(i), Name: "read_file"}},
			RunItem{Type: RunItemToolOutput, Agent: &Agent{Name: "a"}, ToolOutput: &ToolOutputData{CallID: callID(i), Content: strings.Repeat("file content ", 60)}},
		)
	}
	return items
}

func callID(i int) string {
	return "call-" + string(rune('a'+i%26)) + string(rune('0'+i%10))
}

func TestApplyLLMSummaryToPlanUsesModelSummary(t *testing.T) {
	items := manyCompactableItems(30)
	cfg := CompactionConfig{Enabled: true, TriggerTokens: 100, TargetTokens: 80, PreserveRecentItems: 3, PreserveInitialUserMessages: 1, SummaryBulletLimit: 4}
	plan, _, ok, reason := planRunItemsCompaction(items, cfg)
	if !ok {
		t.Fatalf("planRunItemsCompaction not ok: %s", reason)
	}
	model := &mockModel{responses: []*ModelResponse{{
		Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "Findings: composer renders raw tool args.\nNext: add status strip."}}},
		Usage: Usage{InputTokens: 500, OutputTokens: 40},
	}}}

	rebuilt, usage, llmOK := applyLLMSummaryToPlan(context.Background(), model, "test-model", plan, 0)
	if !llmOK {
		t.Fatal("expected LLM summary to apply")
	}
	if usage.OutputTokens != 40 {
		t.Fatalf("usage not propagated: %+v", usage)
	}
	summary := ExtractCompactionSummary(rebuilt)
	if !strings.Contains(summary, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("summary marker missing: %q", summary)
	}
	if !strings.Contains(summary, "composer renders raw tool args") {
		t.Fatalf("LLM content missing from summary: %q", summary)
	}
	if !strings.Contains(summary, "earlier messages compacted") {
		t.Fatalf("scope line missing from summary: %q", summary)
	}
	// The summarizer request must carry the removed transcript with thinking.
	if len(model.requests) != 1 {
		t.Fatalf("expected 1 summary model call, got %d", len(model.requests))
	}
	req := model.requests[0]
	if !strings.Contains(req.Instructions, "continuation brief") {
		t.Fatalf("summary instructions missing: %q", req.Instructions[:80])
	}
	if len(req.Tools) != 0 {
		t.Fatalf("summary call must not expose tools, got %d", len(req.Tools))
	}
	if len(req.Input) != 1 || !strings.Contains(req.Input[0].Message.Text, "[thinking]") {
		t.Fatal("flattened transcript should include reasoning items")
	}
}

func TestApplyLLMSummaryToPlanFallsBackOnModelError(t *testing.T) {
	items := manyCompactableItems(20)
	cfg := CompactionConfig{Enabled: true, TriggerTokens: 100, TargetTokens: 80, PreserveRecentItems: 3, PreserveInitialUserMessages: 1, SummaryBulletLimit: 4}
	plan, _, ok, _ := planRunItemsCompaction(items, cfg)
	if !ok {
		t.Fatal("plan not ok")
	}
	model := &mockModel{} // no scripted responses -> GetResponse errors
	if _, _, llmOK := applyLLMSummaryToPlan(context.Background(), model, "test-model", plan, 0); llmOK {
		t.Fatal("expected fallback on model error")
	}
	// Deterministic plan stays usable.
	if len(plan.Items) == 0 || ExtractCompactionSummary(plan.Items) == "" {
		t.Fatal("deterministic plan must retain its summary")
	}
}

func TestFlattenRunItemsForSummaryTruncatesMiddleOut(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "HEAD-TASK: fix the composer"}},
	}
	for i := 0; i < 200; i++ {
		items = append(items, RunItem{Type: RunItemMessage, Agent: &Agent{Name: "a"}, Message: &MessageOutput{Text: strings.Repeat("filler ", 100)}})
	}
	items = append(items, RunItem{Type: RunItemMessage, Agent: &Agent{Name: "a"}, Message: &MessageOutput{Text: "TAIL-STATE: about to edit view.go"}})

	out := flattenRunItemsForSummary(items, 10_000)
	if len(out) > 11_000 {
		t.Fatalf("transcript not capped: %d chars", len(out))
	}
	if !strings.Contains(out, "HEAD-TASK") || !strings.Contains(out, "TAIL-STATE") {
		t.Fatal("middle-out truncation must keep head and tail")
	}
	if !strings.Contains(out, "[... transcript truncated ...]") {
		t.Fatal("expected truncation marker")
	}
}
