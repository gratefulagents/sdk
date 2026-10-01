package agent

import (
	"strings"
	"testing"
)

func TestEstimateStringTokensCountsRunes(t *testing.T) {
	if got := estimateStringTokens("  日本語テキスト  "); got != 7/4+1 {
		t.Fatalf("estimateStringTokens(CJK) = %d, want %d", got, 7/4+1)
	}
	if got := estimateStringTokens("   "); got != 0 {
		t.Fatalf("estimateStringTokens(blank) = %d, want 0", got)
	}
}

func TestEstimateRunItemsTokensCountsImages(t *testing.T) {
	img := ImageAttachment{MediaType: "image/png", Data: "aGVsbG8="}
	textOnly := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "look at this"}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "c1", Content: "screenshot taken"}},
	}
	withImages := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "look at this", Images: []ImageAttachment{img, img}}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "c1", Content: "screenshot taken", Images: []ImageAttachment{img}}},
	}
	got := estimateRunItemsTokens(withImages) - estimateRunItemsTokens(textOnly)
	if want := 3 * imageTokenEstimate; got != want {
		t.Fatalf("image token delta = %d, want %d", got, want)
	}
}

func TestPlanRunItemsCompactionAfterMatchesBuiltItems(t *testing.T) {
	items := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "Original task: refactor internal/agent/runner.go"}}}
	for i := 0; i < 15; i++ {
		callID := "call_" + strings.Repeat("y", i+1)
		items = append(items,
			RunItem{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: strings.Repeat("next I will inspect pkg/foo.go ", 6)}},
			RunItem{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: callID, Name: "Read", Input: []byte(`{"path":"pkg/foo.go"}`)}},
			RunItem{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: callID, Content: strings.Repeat("file body ", 30)}},
		)
	}
	for _, target := range []int{50, 400, 900} {
		plan, before, ok, reason := planRunItemsCompaction(items, CompactionConfig{
			Enabled:                     true,
			TriggerTokens:               100,
			TargetTokens:                target,
			PreserveRecentItems:         6,
			PreserveInitialUserMessages: 1,
			SummaryBulletLimit:          3,
		})
		if !ok {
			t.Fatalf("target %d: expected compaction, reason=%s", target, reason)
		}
		if got := estimateRunItemsTokens(plan.Items); got != plan.After {
			t.Fatalf("target %d: plan.After = %d, estimate of built items = %d", target, plan.After, got)
		}
		if plan.After >= before {
			t.Fatalf("target %d: after %d >= before %d", target, plan.After, before)
		}
		if !isLocalCompactionSummary(plan.Items[1]) {
			t.Fatalf("target %d: expected summary after the preserved task, got %+v", target, plan.Items[1])
		}
	}
}

func TestPlanRunItemsCompactionRejectsNegligibleReduction(t *testing.T) {
	huge := RunItem{Type: RunItemMessage, Message: &MessageOutput{Text: "Spec:\n" + strings.Repeat("requirement text ", 4000)}}
	cfg := CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               10000,
		TargetTokens:                8000,
		PreserveRecentItems:         2,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          2,
	}
	turn := func(i int) []RunItem {
		return []RunItem{
			{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: strings.Repeat("working on step ", 40)}},
			{Type: RunItemMessage, Message: &MessageOutput{Text: strings.Repeat("continue please ", 40)}},
		}
	}

	// The protected spec alone exceeds the trigger; removing one turn's
	// worth of items is a negligible gain and must not be accepted.
	items := []RunItem{huge}
	for i := 0; i < 3; i++ {
		items = append(items, turn(i)...)
	}
	if _, before, ok, reason := planRunItemsCompaction(items, cfg); ok || reason != "insufficient-reduction" {
		t.Fatalf("expected insufficient-reduction, got ok=%v reason=%q (before=%d)", ok, reason, before)
	}

	// Once enough history has accumulated, compaction frees a meaningful
	// share again and is accepted.
	for i := 3; i < 40; i++ {
		items = append(items, turn(i)...)
	}
	plan, before, ok, reason := planRunItemsCompaction(items, cfg)
	if !ok {
		t.Fatalf("expected compaction once history grew, reason=%q", reason)
	}
	if before-plan.After < before/compactionMinGainDivisor {
		t.Fatalf("accepted plan gain %d below minimum %d", before-plan.After, before/compactionMinGainDivisor)
	}
}
