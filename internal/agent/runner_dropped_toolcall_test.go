package agent

import (
	"context"
	"strings"
	"testing"
)

// TestRunnerRecoversDroppedToolCalls verifies that a response signalling
// tool_use but arriving with no tool calls (dropped in transit, leaving only a
// preamble) is re-requested with a corrective nudge instead of being finalized.
// This is the failure mode where a sub-agent "completes" on turn 1 having done
// nothing.
func TestRunnerRecoversDroppedToolCalls(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items:      []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "I'll start by exploring the code."}}},
				StopReason: "tool_use",
				Usage:      Usage{InputTokens: 10, OutputTokens: 900},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
				Usage: Usage{InputTokens: 12, OutputTokens: 5},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Instructions: "Be helpful"}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "done" {
		t.Fatalf("FinalText() = %q, want \"done\" (must not finalize on the dropped-tool-call preamble)", result.FinalText())
	}
	if model.callIdx != 2 {
		t.Fatalf("model called %d times, want 2 (one recovery re-request)", model.callIdx)
	}
	if len(model.requests) < 2 {
		t.Fatalf("expected 2 requests, got %d", len(model.requests))
	}
	foundNudge := false
	for _, it := range model.requests[1].Input {
		if it.Type == RunItemMessage && it.Message != nil && strings.Contains(it.Message.Text, "dropped in transit") {
			foundNudge = true
		}
	}
	if !foundNudge {
		t.Fatalf("recovery re-request did not include the corrective nudge; input=%+v", model.requests[1].Input)
	}
}

// TestRunnerDroppedToolCallRecoveryIsBounded verifies that a provider that
// deterministically drops tool calls still terminates: after
// maxToolCallRecoveryAttempts recoveries the run finalizes instead of looping.
func TestRunnerDroppedToolCallRecoveryIsBounded(t *testing.T) {
	dropped := func() *ModelResponse {
		return &ModelResponse{
			Items:      []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "preamble"}}},
			StopReason: "tool_use",
			Usage:      Usage{OutputTokens: 100},
		}
	}
	model := &mockModel{responses: []*ModelResponse{dropped(), dropped(), dropped(), dropped(), dropped()}}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if model.callIdx != maxToolCallRecoveryAttempts+1 {
		t.Fatalf("model called %d times, want %d (bounded recovery)", model.callIdx, maxToolCallRecoveryAttempts+1)
	}
	if result.FinalText() != "preamble" {
		t.Fatalf("FinalText() = %q, want \"preamble\" (bounded fallthrough)", result.FinalText())
	}
}

// TestRunnerDoesNotRecoverNormalFinalAnswer verifies the guard only fires on
// the tool_use stop reason, not on ordinary text completions.
func TestRunnerDoesNotRecoverNormalFinalAnswer(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items:      []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "final answer"}}},
				StopReason: "end_turn",
				Usage:      Usage{OutputTokens: 5},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if model.callIdx != 1 {
		t.Fatalf("model called %d times, want 1 (no spurious recovery)", model.callIdx)
	}
	if result.FinalText() != "final answer" {
		t.Fatalf("FinalText() = %q, want \"final answer\"", result.FinalText())
	}
}

// TestRunnerRecoversDroppedToolCallsStreaming proves the guard fires on the
// streaming path too — the real Copilot/Claude case, where the chat-completions
// conversion reports stop_reason=tool_use (from finish_reason) but the tool
// calls did not survive into content.
func TestRunnerRecoversDroppedToolCallsStreaming(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items:      []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "I'll start by exploring."}}},
				StopReason: "tool_use",
				Usage:      Usage{OutputTokens: 900},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
				Usage: Usage{OutputTokens: 5},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	streamed := runner.RunStreamed(context.Background(), agent, nil, RunConfig{})
	for range streamed.Events {
	}
	if err := streamed.Err(); err != nil {
		t.Fatal(err)
	}
	result := streamed.FinalResult()
	if result == nil || result.FinalText() != "done" {
		t.Fatalf("FinalText() = %v, want \"done\" (streaming recovery)", result)
	}
	if model.callIdx != 2 {
		t.Fatalf("model called %d times, want 2 (one streaming recovery re-request)", model.callIdx)
	}
}
