package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func failingTool(name string) *FunctionTool {
	return &FunctionTool{
		ToolName:        name,
		ToolDescription: "always fails",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			return "", errors.New("simulated failure: disk on fire")
		},
	}
}

func toolCallResponse(callID, name string) *ModelResponse {
	return &ModelResponse{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: callID, Name: name, Input: json.RawMessage(`{}`)}}}}
}

func finalResponse(text string) *ModelResponse {
	return &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: text}}}}
}

func TestConsecutiveToolErrorEscalation(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			toolCallResponse("c1", "broken"),
			toolCallResponse("c2", "broken"),
			toolCallResponse("c3", "broken"), // third consecutive failure → escalation
			finalResponse("giving up gracefully"),
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{failingTool("broken")}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	escalations := 0
	for _, item := range result.NewItems {
		if item.Type == RunItemMessage && item.Message != nil && strings.Contains(item.Message.Text, "tool turns all failed") {
			escalations++
		}
	}
	if escalations != 1 {
		t.Fatalf("expected exactly 1 escalation note, got %d", escalations)
	}
}

func TestConsecutiveToolErrorEscalationDisabled(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			toolCallResponse("c1", "broken"),
			toolCallResponse("c2", "broken"),
			toolCallResponse("c3", "broken"),
			finalResponse("done"),
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{failingTool("broken")}}
	result, err := runner.Run(context.Background(), agent, nil, RunConfig{ConsecutiveToolErrorLimit: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.NewItems {
		if item.Type == RunItemMessage && item.Message != nil && strings.Contains(item.Message.Text, "tool turns all failed") {
			t.Fatal("escalation should be disabled")
		}
	}
}

func TestStopGateBlocksUntilPassing(t *testing.T) {
	okTool := &FunctionTool{
		ToolName:        "fixit",
		ToolDescription: "fixes things",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn:              func(ctx context.Context, _ json.RawMessage) (string, error) { return "fixed", nil },
	}
	model := &mockModel{
		responses: []*ModelResponse{
			finalResponse("answer without tests"),
			toolCallResponse("c1", "fixit"),
			finalResponse("answer with tests"),
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{okTool}}

	gateCalls := 0
	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		StopGate: func(_ context.Context, finalText string) (bool, string) {
			gateCalls++
			if strings.Contains(finalText, "with tests") {
				return true, ""
			}
			return false, "run the tests before finalizing"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "answer with tests" {
		t.Fatalf("got %q", result.FinalText())
	}
	if gateCalls != 2 {
		t.Fatalf("gate should run on both candidates, got %d", gateCalls)
	}
	blocked := false
	for _, item := range result.NewItems {
		if item.Type == RunItemMessage && item.Message != nil && strings.Contains(item.Message.Text, "blocked by the completion gate") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("expected gate-block feedback in history")
	}
}

func TestStopGateBypassedAfterMaxBlocks(t *testing.T) {
	// Model insists on the same answer; gate always blocks. With max 2 blocks,
	// the third candidate finalizes.
	model := &mockModel{
		responses: []*ModelResponse{
			finalResponse("stubborn answer"),
			finalResponse("stubborn answer"),
			finalResponse("stubborn answer"),
		},
	}
	runner := NewRunnerWithModel(model)
	okTool := &FunctionTool{ToolName: "t", ToolDescription: "d", Schema: json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) { return "", nil }}
	agent := &Agent{Name: "test", Tools: []Tool{okTool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		StopGate:          func(context.Context, string) (bool, string) { return false, "never good enough" },
		StopGateMaxBlocks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "stubborn answer" {
		t.Fatalf("expected bypass after max blocks, got %q", result.FinalText())
	}
}

func TestFinalAnswerVerifierSingleRefutationRound(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			finalResponse("first draft"),
			finalResponse("revised draft"),
		},
	}
	runner := NewRunnerWithModel(model)
	okTool := &FunctionTool{ToolName: "t", ToolDescription: "d", Schema: json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) { return "", nil }}
	agent := &Agent{Name: "test", Tools: []Tool{okTool}}

	verifierCalls := 0
	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		FinalAnswerVerifier: func(_ context.Context, finalText string) (string, error) {
			verifierCalls++
			return "VERDICT: REJECTED\n1. missing citation", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifierCalls != 1 {
		t.Fatalf("verifier must run at most once, got %d", verifierCalls)
	}
	if result.FinalText() != "revised draft" {
		t.Fatalf("got %q", result.FinalText())
	}
}
