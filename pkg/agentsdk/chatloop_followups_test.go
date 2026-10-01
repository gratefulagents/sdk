package agentsdk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestChatLoopLaterRoundNilResultKeepsEarlierRoundItems(t *testing.T) {
	// Only one scripted response: the resumed round's first model call fails
	// before it accumulates anything, so the runner returns a nil result.
	model := &scriptedChatModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
			ID:    "mutate_call",
			Name:  "mutate",
			Input: json.RawMessage(`{}`),
		}}}},
	}}
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "mutates",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn:              func(context.Context, json.RawMessage) (string, error) { return "mutated", nil },
	}

	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{tool}},
		RunConfig:    RunConfig{MaxTurns: 3, ToolPolicy: &ToolPolicy{ApprovalRequired: true}},
		ApprovalGate: approvingGate{approved: true},
	}).Run(context.Background())
	if err == nil {
		t.Fatal("expected second-round model error")
	}
	if result == nil {
		t.Fatal("result = nil; earlier round's items were dropped")
	}
	var sawCall, sawOutput bool
	for _, item := range result.NewItems {
		if item.Type == RunItemToolCall && item.ToolCall != nil && item.ToolCall.ID == "mutate_call" {
			sawCall = true
		}
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID == "mutate_call" {
			sawOutput = true
		}
	}
	if !sawCall || !sawOutput {
		t.Fatalf("NewItems missing first round's call/output: %+v", result.NewItems)
	}
	if len(result.FinalHistory) == 0 {
		t.Fatal("FinalHistory empty; want the loop's settled history")
	}
	if result.IsInterrupted() {
		t.Fatal("settled result must not carry resolved interruptions")
	}
}
