package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestRunnerDurableTextBoundaries(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}, Usage: Usage{InputTokens: 3, OutputTokens: 1}}}}
	var got []DurableBoundary
	var checkpoints []DurableCheckpoint
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "worker"}, nil, RunConfig{Durable: &DurableRunConfig{Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
		got = append(got, cp.Boundary)
		checkpoints = append(checkpoints, cp)
		if _, err := json.Marshal(cp); err != nil {
			t.Fatalf("checkpoint is not stable JSON: %v", err)
		}
		return nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "done" {
		t.Fatalf("final = %q", result.FinalText())
	}
	want := []DurableBoundary{DurableBoundaryRunStarted, DurableBoundaryModelPrepared, DurableBoundaryModelCompleted, DurableBoundaryRunCompleted}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boundaries = %v, want %v", got, want)
	}
	for i, cp := range checkpoints {
		if cp.RunID == "" || cp.AttemptID == "" || cp.StepID == "" {
			t.Fatalf("checkpoint %d has empty stable ID: %+v", i, cp)
		}
		if cp.Sequence != uint64(i+1) {
			t.Fatalf("checkpoint %d sequence = %d", i, cp.Sequence)
		}
	}
}

func TestRunnerDurableToolCrashAndCrossProcessResume(t *testing.T) {
	calls := 0
	var propagatedKey string
	tool := &FunctionTool{ToolName: "lookup", ReadOnly: true, Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
		calls++
		propagatedKey = DurableIdempotencyKeyFromContext(ctx)
		return "value", nil
	}}
	firstModel := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call-1", Name: "lookup", Input: json.RawMessage(`{}`)}}}}}}
	var saved DurableCheckpoint
	crash := errors.New("forced crash")
	_, err := NewRunnerWithModel(firstModel).Run(context.Background(), &Agent{Name: "worker", Tools: []Tool{tool}}, nil, RunConfig{Durable: &DurableRunConfig{Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
		if cp.Boundary == DurableBoundaryToolCompleted {
			saved = cp
			return crash
		}
		return nil
	}}})
	if !errors.Is(err, crash) {
		t.Fatalf("error = %v, want forced crash", err)
	}
	if calls != 1 || saved.Boundary != DurableBoundaryToolCompleted {
		t.Fatalf("calls=%d checkpoint=%s", calls, saved.Boundary)
	}
	if propagatedKey == "" || propagatedKey != DurableIdempotencyKey(saved.RunID, "call-1") {
		t.Fatalf("tool idempotency key = %q", propagatedKey)
	}

	secondModel := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "resumed"}}}}}}
	result, err := NewRunnerWithModel(secondModel).Run(context.Background(), &Agent{Name: "worker", Tools: []Tool{tool}}, nil, RunConfig{Durable: &DurableRunConfig{RunID: saved.RunID, AttemptID: "attempt-2", Resume: &saved}})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "resumed" {
		t.Fatalf("final = %q", result.FinalText())
	}
	if calls != 1 {
		t.Fatalf("completed effect replayed: calls=%d", calls)
	}
	if len(secondModel.requests) != 1 || len(secondModel.requests[0].Input) != 2 {
		t.Fatalf("restored history = %+v", secondModel.requests)
	}
}

func TestRunnerDurableCheckpointFailureIsFailClosed(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "must not run"}}}}}}
	failure := errors.New("store unavailable")
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "worker"}, nil, RunConfig{Durable: &DurableRunConfig{Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
		if cp.Boundary == DurableBoundaryModelPrepared {
			return failure
		}
		return nil
	}}})
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if model.callIdx != 0 {
		t.Fatalf("model dispatched despite failed prepared checkpoint")
	}
}

func TestRunnerRefusesUnreconciledEffectCheckpoint(t *testing.T) {
	cp := DurableCheckpoint{SchemaVersion: DurableCheckpointSchemaVersion, RunID: "run-1", AttemptID: "attempt-1", Boundary: DurableBoundaryModelCompleted, AgentName: "worker"}
	model := &mockModel{}
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "worker"}, nil, RunConfig{Durable: &DurableRunConfig{Resume: &cp}})
	if err == nil {
		t.Fatal("expected reconciliation error")
	}
	if model.callIdx != 0 {
		t.Fatal("unreconciled effect was replayed")
	}
}
