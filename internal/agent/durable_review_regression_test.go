package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestApprovedToolCheckpointPreservesInterruptedHistory(t *testing.T) {
	tool := &FunctionTool{ToolName: "write", ReadOnly: true, Approval: true, Fn: func(context.Context, json.RawMessage) (string, error) { return "written", nil }}
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "I will write"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call-1", Name: "write", Input: json.RawMessage(`{}`)}},
	}}}}
	var latest DurableCheckpoint
	durableCfg := &DurableRunConfig{Checkpoint: func(_ context.Context, cp DurableCheckpoint) error { latest = cp; return nil }}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "worker", Tools: []Tool{tool}}
	result, err := runner.Run(context.Background(), agent, []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "please write"}}}, RunConfig{Durable: durableCfg})
	if err != nil {
		t.Fatal(err)
	}
	if result.Interruption == nil || durableCfg.Resume == nil || durableCfg.Resume.Boundary != DurableBoundaryApprovalPending {
		t.Fatalf("interruption=%+v resume=%+v", result.Interruption, durableCfg.Resume)
	}
	if _, _, _, _, err := runner.ExecuteApprovedTool(context.Background(), agent, ToolCallData{ID: "call-1", Name: "write", Input: json.RawMessage(`{}`)}, RunConfig{Durable: durableCfg}); err != nil {
		t.Fatal(err)
	}
	if latest.Boundary != DurableBoundaryToolCompleted {
		t.Fatalf("boundary=%s", latest.Boundary)
	}
	var call, output bool
	for _, item := range latest.History {
		if item.ToolCall != nil && item.ToolCall.ID == "call-1" {
			call = true
		}
		if item.ToolOutput != nil && item.ToolOutput.CallID == "call-1" {
			output = true
		}
	}
	if !call || !output || len(latest.History) < 5 {
		t.Fatalf("history lost around approval: %+v", latest.History)
	}
}

func TestCompletedCheckpointDoesNotRunModelAgain(t *testing.T) {
	cp := DurableCheckpoint{SchemaVersion: DurableCheckpointSchemaVersion, RunID: "run-1", AttemptID: "attempt-1", Boundary: DurableBoundaryRunCompleted, AgentName: "worker", History: SnapshotRunItems([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}})}
	model := &mockModel{}
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "worker"}, nil, RunConfig{Durable: &DurableRunConfig{Resume: &cp}})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "done" || model.callIdx != 0 {
		t.Fatalf("result=%+v model calls=%d", result, model.callIdx)
	}
}

func TestStopOnToolEmitsRunCompletedCheckpoint(t *testing.T) {
	tool := &FunctionTool{ToolName: "lookup", ReadOnly: true, Fn: func(context.Context, json.RawMessage) (string, error) { return "value", nil }}
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call-1", Name: "lookup", Input: json.RawMessage(`{}`)}}}}}}
	var latest DurableCheckpoint
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "worker", Tools: []Tool{tool}, ToolUseBehavior: StopOnFirstTool}, nil, RunConfig{Durable: &DurableRunConfig{Checkpoint: func(_ context.Context, cp DurableCheckpoint) error { latest = cp; return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || latest.Boundary != DurableBoundaryRunCompleted {
		t.Fatalf("result=%+v boundary=%s", result, latest.Boundary)
	}
}
