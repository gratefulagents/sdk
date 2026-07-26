package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRunnerDurableCancellationBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var boundaries []DurableBoundary
	runner, agent := NewRunnerWithModel(&mockModel{}), &Agent{Name: "worker"}
	_, err := runner.Run(ctx, agent, nil, RunConfig{Durable: &DurableRunConfig{Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
		boundaries = append(boundaries, cp.Boundary)
		return nil
	}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if len(boundaries) == 0 || boundaries[len(boundaries)-1] != DurableBoundaryRunCancelled {
		t.Fatalf("boundaries=%v", boundaries)
	}
}

func TestRunnerForcedCrashAtEveryDurableBoundary(t *testing.T) {
	forced := errors.New("forced process crash")
	text := func() (*Runner, *Agent) {
		return NewRunnerWithModel(&mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}}}}), &Agent{Name: "worker"}
	}
	tool := func(name string, approval bool) func() (*Runner, *Agent) {
		return func() (*Runner, *Agent) {
			model := &mockModel{responses: []*ModelResponse{
				{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call-1", Name: name, Input: json.RawMessage(`{}`)}}}},
				{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
			}}
			fn := &FunctionTool{ToolName: name, ReadOnly: true, Approval: approval, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
			return NewRunnerWithModel(model), &Agent{Name: "worker", Tools: []Tool{fn}}
		}
	}
	handoff := func() (*Runner, *Agent) {
		target := &Agent{Name: "specialist"}
		h := NewHandoff(target)
		model := &mockModel{responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "handoff-1", Name: h.ToolName, Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		}}
		return NewRunnerWithModel(model), &Agent{Name: "front", Handoffs: []*Handoff{h}}
	}
	cases := []struct {
		boundary DurableBoundary
		setup    func() (*Runner, *Agent)
	}{
		{DurableBoundaryRunStarted, text},
		{DurableBoundaryModelPrepared, text},
		{DurableBoundaryModelCompleted, text},
		{DurableBoundaryRunCompleted, text},
		{DurableBoundaryToolPrepared, tool("lookup", false)},
		{DurableBoundaryToolCompleted, tool("lookup", false)},
		{DurableBoundaryApprovalPending, tool("write", true)},
		{DurableBoundaryHandoffCompleted, handoff},
		{DurableBoundaryPaused, tool("AskUserQuestion", false)},
	}
	for _, tc := range cases {
		t.Run(string(tc.boundary), func(t *testing.T) {
			runner, agent := tc.setup()
			seen := false
			_, err := runner.Run(context.Background(), agent, nil, RunConfig{MaxTurns: 3, Durable: &DurableRunConfig{Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
				if cp.Boundary == tc.boundary {
					seen = true
					return forced
				}
				return nil
			}}})
			if !seen || !errors.Is(err, forced) {
				t.Fatalf("seen=%v error=%v", seen, err)
			}
		})
	}
}
