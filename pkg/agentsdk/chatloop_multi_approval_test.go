package agentsdk

// Regression test for the 2026-07 audit: a turn with several parallel
// approval-needing tool calls must resolve ALL of them before resuming —
// leaving one pending produces an unpaired tool_use the provider rejects.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type recordingGate struct {
	mu    sync.Mutex
	names []string
}

func (g *recordingGate) ApproveTool(_ context.Context, req ToolApprovalRequest) (bool, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.names = append(g.names, req.ToolName)
	return true, "", nil
}

func TestChatLoopResolvesAllParallelToolApprovals(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{
		{Items: []RunItem{
			{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "mutate_a", Input: json.RawMessage(`{}`)}},
			{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "mutate_b", Input: json.RawMessage(`{}`)}},
		}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "both done"}}}},
	}}

	var executed []string
	var mu sync.Mutex
	makeTool := func(name string) *FunctionTool {
		return &FunctionTool{
			ToolName: name,
			Schema:   json.RawMessage(`{"type":"object"}`),
			Fn: func(context.Context, json.RawMessage) (string, error) {
				mu.Lock()
				executed = append(executed, name)
				mu.Unlock()
				return "ok", nil
			},
		}
	}

	gate := &recordingGate{}
	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{makeTool("mutate_a"), makeTool("mutate_b")}},
		RunConfig:    RunConfig{MaxTurns: 3, ToolPolicy: &ToolPolicy{ApprovalRequired: true}},
		ApprovalGate: gate,
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "both done" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
	if len(gate.names) != 2 {
		t.Fatalf("approval gate consulted %d times (%v), want 2", len(gate.names), gate.names)
	}
	if len(executed) != 2 {
		t.Fatalf("executed tools = %v, want both", executed)
	}

	// Every tool_use in the resume history must be paired with an output.
	outputs := map[string]bool{}
	for _, item := range result.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil {
			outputs[item.ToolOutput.CallID] = true
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if !outputs[id] {
			t.Fatalf("tool call %s has no output in history; next model call would be rejected. items=%+v", id, result.NewItems)
		}
	}
}
