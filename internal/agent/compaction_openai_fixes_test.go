package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// mockCompactorModel wraps mockModel with a provider-compaction API so tests
// can observe when the runner asks the provider to compact.
type mockCompactorModel struct {
	mockModel
	compactCalls   int
	compactResults []*CompactionResult
}

func (m *mockCompactorModel) SupportsContextCompaction() bool { return true }

func (m *mockCompactorModel) CompactContext(_ context.Context, _ ModelRequest) (*CompactionResult, error) {
	m.compactCalls++
	if len(m.compactResults) == 0 {
		return nil, errors.New("no compact results configured")
	}
	result := m.compactResults[0]
	if len(m.compactResults) > 1 {
		m.compactResults = m.compactResults[1:]
	}
	return result, nil
}

// A compaction blob's byte size must not be counted as chars/4 tokens: real
// blobs run 100KB-10MB, and an uncapped estimate keeps the post-compaction
// history above the trigger forever (re-compaction every turn).
func TestEstimateRunItemsTokensCapsCompactionBlob(t *testing.T) {
	blob := strings.Repeat("x", 1_000_000)
	items := []RunItem{{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: blob}}}
	got := estimateRunItemsTokens(items)
	if got > compactionItemTokenEstimateCap+16 {
		t.Fatalf("estimate = %d, want capped at ~%d", got, compactionItemTokenEstimateCap)
	}
	if got < 1000 {
		t.Fatalf("estimate = %d, want a non-trivial cost for a large blob", got)
	}
}

// Once a provider compaction item anchors the history, only plaintext growth
// after it may re-trigger the pre-request compaction pass. The blob itself
// (however large its estimate) must not cause a compaction storm.
func TestShouldDeferCompactionForBlobGrowth(t *testing.T) {
	blob := strings.Repeat("x", 500_000)
	cfg := CompactionConfig{Enabled: true, TriggerTokens: 1000, TargetTokens: 500}
	items := []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: blob}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "small new user message"}},
	}
	if !shouldDeferCompactionForBlobGrowth(items, cfg, 0) {
		t.Fatal("expected defer while post-blob growth is below the trigger")
	}
	grown := append(append([]RunItem(nil), items...), RunItem{
		Type: RunItemMessage, Message: &MessageOutput{Text: strings.Repeat("growth ", 2000)},
	})
	if shouldDeferCompactionForBlobGrowth(grown, cfg, 0) {
		t.Fatal("expected no defer once post-blob growth crosses the trigger")
	}
	noBlob := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: strings.Repeat("growth ", 2000)}}}
	if shouldDeferCompactionForBlobGrowth(noBlob, cfg, 0) {
		t.Fatal("expected no defer without a provider compaction item")
	}
}

// In the deferred steady state the runner must skip BOTH provider and local
// compaction: the provider blob stays in the request untouched and no local
// summary replaces it (a textual digest of an opaque blob preserves nothing).
func TestRunnerDefersAllCompactionInBlobSteadyState(t *testing.T) {
	blob := strings.Repeat("x", 500_000)
	model := &mockCompactorModel{
		mockModel: mockModel{
			responses: []*ModelResponse{
				{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	_, err := runner.Run(context.Background(), &Agent{Name: "test"}, []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: blob, CreatedBy: "mock"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "small follow-up"}},
	}, RunConfig{
		// Trigger sits above the fixed request overhead (~24.6K: output
		// reserve + safety buffer) but below overhead + capped blob estimate,
		// so pre-fix this would have re-compacted; post-fix it defers.
		CompactionConfig: CompactionConfig{Enabled: true, TriggerTokens: 30_000, TargetTokens: 15_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.compactCalls != 0 {
		t.Fatalf("provider compact calls = %d, want 0 (deferred)", model.compactCalls)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	var blobSurvived bool
	for _, item := range model.requests[0].Input {
		if item.Type == RunItemCompaction && item.Compaction != nil && item.Compaction.EncryptedContent == blob {
			blobSurvived = true
		}
		if item.Type == RunItemMessage && item.Agent != nil && item.Agent.Name == "context-summary" {
			t.Fatalf("local compaction summary replaced the provider blob: %+v", item)
		}
	}
	if !blobSurvived {
		t.Fatalf("provider compaction blob missing from request input: %+v", model.requests[0].Input)
	}
}

// Real post-blob growth above the trigger still compacts via the provider.
func TestRunnerRecompactsOnRealPostBlobGrowth(t *testing.T) {
	blob := strings.Repeat("x", 500_000)
	model := &mockCompactorModel{
		mockModel: mockModel{
			responses: []*ModelResponse{
				{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
			},
		},
		compactResults: []*CompactionResult{{
			Items: []RunItem{{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_2", EncryptedContent: "fresh-blob"}}},
		}},
	}
	runner := NewRunnerWithModel(model)
	_, err := runner.Run(context.Background(), &Agent{Name: "test"}, []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: blob, CreatedBy: "mock"}},
		// ~8.7K tokens of post-blob plaintext + ~24.6K request overhead
		// crosses the 30K trigger on growth alone.
		{Type: RunItemMessage, Message: &MessageOutput{Text: strings.Repeat("growth ", 5000)}},
	}, RunConfig{
		CompactionConfig: CompactionConfig{Enabled: true, TriggerTokens: 30_000, TargetTokens: 15_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.compactCalls != 1 {
		t.Fatalf("provider compact calls = %d, want 1", model.compactCalls)
	}
}

// The local planner must never remove a provider compaction item: it is the
// only copy of the provider-compacted history. Covers the provider-error
// fallback and the forced overflow-recovery pass (trigger=1).
func TestLocalCompactionProtectsProviderBlob(t *testing.T) {
	items := []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "encrypted-blob"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the user task"}},
	}
	for i := 0; i < 40; i++ {
		items = append(items, RunItem{
			Type:  RunItemMessage,
			Agent: &Agent{Name: "assistant"},
			Message: &MessageOutput{
				Text: strings.Repeat("assistant progress notes ", 40),
			},
		})
	}
	for _, trigger := range []int{1, 100} {
		cfg := CompactionConfig{
			Enabled:                     true,
			TriggerTokens:               trigger,
			TargetTokens:                maxInt(trigger/2, 1),
			PreserveRecentItems:         2,
			PreserveInitialUserMessages: 1,
			SummaryBulletLimit:          4,
		}
		plan, _, ok, reason := planRunItemsCompaction(items, cfg)
		if !ok {
			t.Fatalf("trigger=%d: planRunItemsCompaction not ok: %s", trigger, reason)
		}
		var blobKept bool
		for _, item := range plan.Items {
			if item.Type == RunItemCompaction && item.Compaction != nil && item.Compaction.EncryptedContent == "encrypted-blob" {
				blobKept = true
			}
		}
		if !blobKept {
			t.Fatalf("trigger=%d: local compaction removed the provider blob: %+v", trigger, plan.Items)
		}
		for _, removed := range plan.Removed {
			if removed.Type == RunItemCompaction {
				t.Fatalf("trigger=%d: provider blob listed in removed items", trigger)
			}
		}
	}
}

// Provider compaction output is typically just an opaque encrypted blob. The
// original task prompt must survive in plaintext, mirroring the local path's
// PreserveInitialUserMessages guarantee.
func TestApplyCompactionCarryForwardPreservesInitialUserMessages(t *testing.T) {
	previous := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "original task: fix the flaky auth test"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "follow-up constraint: do not touch the fixtures"}},
	}
	for i := 0; i < 30; i++ {
		previous = append(previous,
			RunItem{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call_" + string(rune('a'+i)), Name: "Bash", Input: json.RawMessage(`{}`)}},
			RunItem{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "call_" + string(rune('a'+i)), Content: "output"}},
		)
	}
	compacted := []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "encrypted-state"}},
	}

	got, _ := applyCompactionCarryForward(context.Background(), compacted, previous, RunConfig{
		CompactionConfig: CompactionConfig{
			Enabled:                     true,
			PreserveRecentItems:         4,
			PreserveInitialUserMessages: 2,
		},
	}, CompactionConfig{
		Enabled:                     true,
		PreserveRecentItems:         4,
		PreserveInitialUserMessages: 2,
	}, 0)

	if len(got) == 0 || got[0].Type != RunItemCompaction {
		t.Fatalf("first item = %+v, want compaction blob", got)
	}
	text := Items.ExtractText(got)
	if !strings.Contains(text, "original task: fix the flaky auth test") {
		t.Fatalf("post-compaction input lost the original task: %q", text)
	}
	if !strings.Contains(text, "follow-up constraint") {
		t.Fatalf("post-compaction input lost the second user message: %q", text)
	}
	// The initial user messages sit after the blob, before the recent tail.
	taskIdx, tailIdx := -1, -1
	for i, item := range got {
		if item.Type == RunItemMessage && item.Message != nil && strings.HasPrefix(item.Message.Text, "original task") {
			taskIdx = i
		}
		if item.Type == RunItemToolOutput && tailIdx < 0 {
			tailIdx = i
		}
	}
	if taskIdx < 1 || (tailIdx >= 0 && taskIdx > tailIdx) {
		t.Fatalf("task position = %d, first tail item = %d; want blob < task < tail", taskIdx, tailIdx)
	}
}

// A provider that rejects stale/foreign encrypted context (OpenAI
// invalid_encrypted_content) must not brick the run: the runner strips the
// undecryptable items and retries with the surviving plaintext.
func TestRunnerStripsUndecryptableEncryptedItemsAndRetries(t *testing.T) {
	model := &mockModel{
		errors: []error{errors.New(`request failed: 400 {"error":{"code":"invalid_encrypted_content","message":"The encrypted content gAAA... could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}`)},
		responses: []*ModelResponse{
			nil,
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "recovered and done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	result, err := runner.Run(context.Background(), &Agent{Name: "test"}, []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_stale", EncryptedContent: "stale-blob"}},
		{Type: RunItemReasoning, Reasoning: &ReasoningData{ID: "rs_1", EncryptedContent: "stale-reasoning"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the actual task"}},
	}, RunConfig{})
	if err != nil {
		t.Fatalf("run failed instead of recovering: %v", err)
	}
	if got := finalOutputText(result.FinalOutput); !strings.Contains(got, "recovered and done") {
		t.Fatalf("final output = %q", got)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2 (failed + retried)", len(model.requests))
	}
	for _, item := range model.requests[1].Input {
		if item.Type == RunItemCompaction {
			t.Fatalf("retry still contains the rejected compaction blob: %+v", item)
		}
		if item.Type == RunItemReasoning && item.Reasoning != nil && item.Reasoning.EncryptedContent != "" {
			t.Fatalf("retry still contains encrypted reasoning: %+v", item)
		}
	}
	if text := Items.ExtractText(model.requests[1].Input); !strings.Contains(text, "the actual task") {
		t.Fatalf("retry lost the plaintext task: %q", text)
	}
}

func TestIsEncryptedContentError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{`400 {"error":{"code":"invalid_encrypted_content"}}`, true},
		{`The encrypted content gAAA could not be verified. Reason: Encrypted content could not be decrypted or parsed.`, true},
		{`encrypted_content could not be decrypted`, true},
		{`context_length_exceeded`, false},
		{`Invalid 'input[3].encrypted_content': string too long`, true},
		{`Invalid 'messages[2].content': string too long`, false},
	}
	for _, tc := range cases {
		if got := isEncryptedContentError(errors.New(tc.msg)); got != tc.want {
			t.Errorf("isEncryptedContentError(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// The context_management compact_threshold sent to the provider must stay at
// the model's real (resolver-provided) trigger: the provider compares it with
// REAL token counts, so the local estimator calibration must not shrink it.
func TestModelRequestCompactionThresholdNotCalibrationShrunk(t *testing.T) {
	model := &mockCompactorModel{
		mockModel: mockModel{
			responses: []*ModelResponse{
				{
					Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call1", Name: "echo", Input: json.RawMessage(`"hi"`)}}},
					// Actual usage far above the naive estimate inflates
					// estimateCalibration to its 2.5 clamp.
					Usage: Usage{InputTokens: 500_000},
				},
				{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
			},
		},
	}
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			return "echoed", nil
		},
	}
	runner := NewRunnerWithModel(model)
	_, err := runner.Run(context.Background(), &Agent{Name: "test", Tools: []Tool{echoTool}}, []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "task"}},
	}, RunConfig{
		CompactionConfig: CompactionConfig{Enabled: true, TriggerTokens: 100_000, TargetTokens: 50_000},
		CompactionModelResolver: func(context.Context, string) (int, int, bool) {
			return 100_000, 50_000, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	for i, req := range model.requests {
		if req.CompactionThreshold != 100_000 {
			t.Fatalf("request %d CompactionThreshold = %d, want uncalibrated 100000", i, req.CompactionThreshold)
		}
	}
}

// Compaction items arriving without created_by are stamped with the producing
// provider so request builders can skip foreign blobs after model switches.
func TestStampCompactionItemOrigin(t *testing.T) {
	model := &mockCompactorModel{}
	items := []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "blob"}},
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_2", EncryptedContent: "blob", CreatedBy: "openai"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "not a compaction"}},
	}
	stampCompactionItemOrigin(items, model)
	if got := items[0].Compaction.CreatedBy; got != "mock" {
		t.Fatalf("unstamped item CreatedBy = %q, want provider stamp", got)
	}
	if got := items[1].Compaction.CreatedBy; got != "openai" {
		t.Fatalf("pre-set CreatedBy overwritten: %q", got)
	}
}

// Reasoning items in the preserved tail must survive provider compaction:
// runItemDedupeKey previously returned "" for RunItemReasoning, so
// appendMissingRecentRunItems silently dropped every reasoning item — on the
// Codex backend that discards the model's encrypted working memory and leaves
// function_calls without their paired rs_ items (run chat-gf-all-aqvafl).
func TestCarryForwardPreservesReasoningItemsInTail(t *testing.T) {
	agentRef := &Agent{Name: "assistant"}
	previous := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the task"}},
	}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("call_%d", i)
		previous = append(previous,
			RunItem{Type: RunItemReasoning, Agent: agentRef, Reasoning: &ReasoningData{ID: fmt.Sprintf("rs_%d", i), EncryptedContent: "enc-" + id}},
			RunItem{Type: RunItemToolCall, Agent: agentRef, ToolCall: &ToolCallData{ID: id, Name: "read_file", Input: json.RawMessage(`{}`)}},
			RunItem{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: id, Content: "file content"}},
		)
	}
	compacted := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the task"}},
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "blob"}},
	}
	got, _ := applyCompactionCarryForward(context.Background(), compacted, previous, RunConfig{
		CompactionConfig: CompactionConfig{Enabled: true, PreserveRecentItems: 6, PreserveInitialUserMessages: 1},
	}, CompactionConfig{Enabled: true, PreserveRecentItems: 6, PreserveInitialUserMessages: 1}, 0)

	var reasoning, calls int
	for _, item := range got {
		switch item.Type {
		case RunItemReasoning:
			reasoning++
		case RunItemToolCall:
			calls++
		}
	}
	if reasoning == 0 {
		t.Fatalf("post-compaction input lost all reasoning items: %+v", got)
	}
	if calls == 0 {
		t.Fatalf("post-compaction input lost the recent tool calls")
	}
}

// A stale carry-forward message from an earlier compaction must not be
// re-inserted as an "initial user message" on the next compaction: the fresh
// carry-forward appended at the end is the only copy allowed to survive.
func TestCarryForwardNotDuplicatedAcrossCompactions(t *testing.T) {
	previous := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the task"}},
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "old-blob"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "[COMPACTION CARRY-FORWARD]\nstale runtime state from the previous compaction"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "later plain user message"}},
	}
	compacted := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the task"}},
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_2", EncryptedContent: "new-blob"}},
	}
	got, carryForward := applyCompactionCarryForward(context.Background(), compacted, previous, RunConfig{
		CompactionConfig:    CompactionConfig{Enabled: true, PreserveRecentItems: 2, PreserveInitialUserMessages: 2},
		WorkingStateContext: "fresh runtime state",
	}, CompactionConfig{Enabled: true, PreserveRecentItems: 2, PreserveInitialUserMessages: 2}, 0)
	if !strings.Contains(carryForward, "fresh runtime state") {
		t.Fatalf("carry-forward = %q, want fresh runtime state", carryForward)
	}
	var carryForwards, stale int
	for _, item := range got {
		if isCompactionCarryForwardItem(item) {
			carryForwards++
			if strings.Contains(item.Message.Text, "stale runtime state") {
				stale++
			}
		}
	}
	if stale != 0 {
		t.Fatalf("stale carry-forward re-inserted after re-compaction: %+v", got)
	}
	if carryForwards != 1 {
		t.Fatalf("carry-forward count = %d, want exactly 1 (the fresh one)", carryForwards)
	}
}

// The preserved tail extends beyond PreserveRecentItems while it fits the
// compaction target: a provider blob compresses the whole history into a few
// KB, and refilling only ~3 tool exchanges left the post-compaction window
// nearly empty while the agent lost its recent working state and restarted
// discovery (run chat-gf-all-aqvafl re-ran greps/reads it had already done).
func TestCarryForwardTailUsesTargetTokenBudget(t *testing.T) {
	agentRef := &Agent{Name: "assistant"}
	previous := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the task"}},
	}
	const turns = 30
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("call_%d", i)
		previous = append(previous,
			RunItem{Type: RunItemToolCall, Agent: agentRef, ToolCall: &ToolCallData{ID: id, Name: "read_file", Input: json.RawMessage(`{}`)}},
			RunItem{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: id, Content: strings.Repeat("x", 4000)}}, // ~1000 tokens each
		)
	}
	compacted := []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "blob"}},
	}
	cfg := RunConfig{CompactionConfig: CompactionConfig{
		Enabled: true, TriggerTokens: 60_000, TargetTokens: 20_000,
		PreserveRecentItems: 4, PreserveInitialUserMessages: 1,
	}}

	// Large remaining budget: the tail should reach well past 4 items.
	got, _ := applyCompactionCarryForward(context.Background(), compacted, previous, cfg, cfg.CompactionConfig, 0)
	var outputs int
	for _, item := range got {
		if item.Type == RunItemToolOutput {
			outputs++
		}
	}
	if outputs <= 4 {
		t.Fatalf("budgeted tail outputs = %d, want > PreserveRecentItems/2 pairs", outputs)
	}
	if outputs == turns {
		t.Fatalf("tail unexpectedly kept the whole history; budget not applied")
	}

	// Overhead consuming the whole target: fall back to the minimum item count.
	gotMin, _ := applyCompactionCarryForward(context.Background(), compacted, previous, cfg, cfg.CompactionConfig, 60_000)
	outputs = 0
	var callsMin int
	for _, item := range gotMin {
		switch item.Type {
		case RunItemToolOutput:
			outputs++
		case RunItemToolCall:
			callsMin++
		}
	}
	if outputs != 2 || callsMin != 2 {
		t.Fatalf("min tail = %d calls / %d outputs, want 2/2 (PreserveRecentItems=4)", callsMin, outputs)
	}
}
