package agent

import (
	"context"
	"encoding/json"
	"errors"
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
// after it may re-trigger provider compaction. The blob itself (however large
// its estimate) must not cause a compaction storm.
func TestProviderCompactionNotRetriggeredByBlobEstimate(t *testing.T) {
	model := &mockCompactorModel{}
	blob := strings.Repeat("x", 500_000)
	req := ModelRequest{
		Model: "gpt-test",
		Input: []RunItem{
			{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: blob}},
			{Type: RunItemMessage, Message: &MessageOutput{Text: "small new user message"}},
		},
	}
	cfg := CompactionConfig{Enabled: true, TriggerTokens: 1000, TargetTokens: 500}

	_, _, _, ok, err := compactRunItemsWithModelAPI(context.Background(), model, req, 0, cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || model.compactCalls != 0 {
		t.Fatalf("compaction fired on blob estimate alone (ok=%v calls=%d), want skip", ok, model.compactCalls)
	}

	// Genuine post-compaction growth above the trigger still compacts.
	model.compactResults = []*CompactionResult{{
		Items: []RunItem{{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_2", EncryptedContent: "fresh-blob"}}},
	}}
	req.Input = append(req.Input, RunItem{Type: RunItemMessage, Message: &MessageOutput{Text: strings.Repeat("growth ", 2000)}})
	_, _, _, ok, err = compactRunItemsWithModelAPI(context.Background(), model, req, 0, cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || model.compactCalls != 1 {
		t.Fatalf("compaction did not fire on real growth (ok=%v calls=%d)", ok, model.compactCalls)
	}

	// force=true (context-overflow recovery) bypasses the growth guard.
	model.compactResults = []*CompactionResult{{
		Items: []RunItem{{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_3", EncryptedContent: "forced-blob"}}},
	}}
	req.Input = req.Input[:2]
	_, _, _, ok, err = compactRunItemsWithModelAPI(context.Background(), model, req, 0, cfg, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || model.compactCalls != 2 {
		t.Fatalf("forced compaction skipped (ok=%v calls=%d)", ok, model.compactCalls)
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
	})

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
