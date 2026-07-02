package agent

// Regression tests for the 2026-07 full logic audit fixes:
//   - multiple parallel approval interruptions surfaced (Interruptions slice)
//   - handoff InputFilter sees the current turn's items
//   - handoff sibling tool calls receive synthesized outputs
//   - emitRunItems respects context cancellation (no leaked run goroutine)
//   - ineffective compaction summaries are rejected
//   - panicking sub-agent model fails the task instead of crashing the host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func approvalTool(name string) *FunctionTool {
	return &FunctionTool{
		ToolName: name,
		Schema:   json.RawMessage(`{"type":"object","properties":{}}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "ran " + name, nil
		},
		Approval: true,
	}
}

func TestRunnerSurfacesAllParallelApprovalInterruptions(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "danger_a", Input: json.RawMessage(`{}`)}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "danger_b", Input: json.RawMessage(`{}`)}},
			}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:  "test",
		Tools: []Tool{approvalTool("danger_a"), approvalTool("danger_b")},
	}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsInterrupted() {
		t.Fatal("expected interrupted result")
	}
	if len(result.Interruptions) != 2 {
		t.Fatalf("Interruptions = %d, want 2", len(result.Interruptions))
	}
	if result.Interruption == nil || result.Interruption.ToolCallID != result.Interruptions[0].ToolCallID {
		t.Fatalf("Interruption should alias the first pending interruption: %+v", result.Interruption)
	}
	got := map[string]bool{}
	for _, in := range result.Interruptions {
		got[in.ToolName] = true
	}
	if !got["danger_a"] || !got["danger_b"] {
		t.Fatalf("interruptions = %+v, want danger_a and danger_b", result.Interruptions)
	}
	if len(result.AllInterruptions()) != 2 {
		t.Fatalf("AllInterruptions() = %d, want 2", len(result.AllInterruptions()))
	}
}

func TestHandoffInputFilterSeesCurrentTurnItems(t *testing.T) {
	target := &Agent{Name: "specialist"}
	handoff := NewHandoff(target)

	var filterInput []RunItem
	handoff.InputFilter = func(input []RunItem, history []RunItem) []RunItem {
		filterInput = append([]RunItem(nil), input...)
		return input
	}

	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{
				{Type: RunItemMessage, Message: &MessageOutput{Text: "let me transfer you"}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "h1", Name: handoff.ToolName, Input: json.RawMessage(`{}`)}},
			}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "front", Handoffs: []*Handoff{handoff}}

	result, err := runner.Run(context.Background(), agent, []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "hi"}}}, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "done" {
		t.Fatalf("FinalText() = %q, want done", result.FinalText())
	}

	var sawTransferMessage, sawHandoffCall, sawHandoffOutput bool
	for _, item := range filterInput {
		switch {
		case item.Type == RunItemMessage && item.Message != nil && item.Message.Text == "let me transfer you":
			sawTransferMessage = true
		case item.Type == RunItemToolCall && item.ToolCall != nil && item.ToolCall.ID == "h1":
			sawHandoffCall = true
		case item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID == "h1":
			sawHandoffOutput = true
		}
	}
	if !sawTransferMessage || !sawHandoffCall || !sawHandoffOutput {
		t.Fatalf("filter input missing current-turn items (msg=%v call=%v output=%v): %+v",
			sawTransferMessage, sawHandoffCall, sawHandoffOutput, filterInput)
	}
}

func TestHandoffSynthesizesOutputsForSiblingToolCalls(t *testing.T) {
	target := &Agent{Name: "specialist"}
	handoff := NewHandoff(target)

	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "t1", Name: "grep", Input: json.RawMessage(`{}`)}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "h1", Name: handoff.ToolName, Input: json.RawMessage(`{}`)}},
			}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:     "front",
		Handoffs: []*Handoff{handoff},
		Tools: []Tool{&FunctionTool{
			ToolName: "grep",
			Schema:   json.RawMessage(`{"type":"object","properties":{}}`),
			Fn:       func(context.Context, json.RawMessage) (string, error) { return "found", nil },
			ReadOnly: true,
		}},
	}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]*ToolOutputData{}
	for _, item := range result.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil {
			outputs[item.ToolOutput.CallID] = item.ToolOutput
		}
	}
	if outputs["h1"] == nil || !strings.Contains(outputs["h1"].Content, "Handing off to specialist") {
		t.Fatalf("handoff call output missing/wrong: %+v", outputs["h1"])
	}
	sibling := outputs["t1"]
	if sibling == nil {
		t.Fatalf("sibling tool call t1 has no synthesized output; unpaired tool_use would be rejected by providers. outputs=%+v", outputs)
	}
	if !sibling.IsError || !strings.Contains(sibling.Content, "handed off") {
		t.Fatalf("sibling output should explain the handoff skip, got %+v", sibling)
	}
}

func TestRunStreamedFinalResultUnblocksOnCancelWithoutDrainingEvents(t *testing.T) {
	// Produce far more run items than the 64-slot event buffer while the
	// consumer never drains Events. Cancelling the context must still let the
	// run goroutine exit and FinalResult return.
	var responses []*ModelResponse
	for i := 0; i < 40; i++ {
		responses = append(responses, &ModelResponse{Items: []RunItem{
			{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c", Name: "noop", Input: json.RawMessage(`{}`)}},
		}})
	}
	model := &mockModel{responses: responses}
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name: "test",
		Tools: []Tool{&FunctionTool{
			ToolName: "noop",
			Schema:   json.RawMessage(`{"type":"object","properties":{}}`),
			Fn: func(context.Context, json.RawMessage) (string, error) {
				return strings.Repeat("x", 10), nil
			},
			ReadOnly: true,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	streamed := runner.RunStreamed(ctx, agent, nil, RunConfig{MaxTurns: 60})

	time.Sleep(50 * time.Millisecond) // let the buffer fill
	cancel()

	doneCh := make(chan *RunResult, 1)
	go func() { doneCh <- streamed.FinalResult() }()
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("FinalResult did not return after cancellation; run goroutine is stuck on the events channel")
	}
}

func TestCompactionRejectsSummaryThatGrowsTokens(t *testing.T) {
	// Several tiny removable items whose generated summary is larger than the
	// removed content. Compaction must report ok=false instead of pretending
	// the input shrank.
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "a"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "b", Input: json.RawMessage(`{}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "c1", Content: "c"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "recent tail"}},
	}
	cfg := CompactionConfig{
		Enabled:             true,
		TriggerTokens:       1,
		TargetTokens:        1,
		PreserveRecentItems: 1,
	}
	compacted, _, after, ok, reason := MaybeCompactRunItems(items, cfg)
	if ok {
		before := estimateRunItemsTokens(items)
		if after >= before {
			t.Fatalf("compaction reported ok with after=%d >= before=%d (items=%d -> %d)",
				after, before, len(items), len(compacted))
		}
	} else if reason != "ineffective-summary" && reason != "no-removable-history" && reason != "below-threshold" {
		t.Fatalf("unexpected rejection reason %q", reason)
	}
}

// panicModel panics inside GetResponse to simulate an orchestration-level
// failure in a sub-agent run.
type panicModel struct{}

func (panicModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	panic("orchestration bug")
}
func (panicModel) StreamResponse(context.Context, ModelRequest) (*ModelStream, error) {
	panic("orchestration bug")
}
func (panicModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (panicModel) CalculateCost(Usage) float64            { return 0 }
func (panicModel) Provider() string                       { return "mock" }

func TestSubAgentTaskPanicFailsTaskInsteadOfCrashing(t *testing.T) {
	runner := NewRunnerWithModel(panicModel{})
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: runner,
		Agents: map[string]*Agent{"child": {Name: "child"}},
	})

	taskID, err := registry.SpawnAsync(context.Background(), "child", "do work", "")
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("task never reached a terminal state after panic")
		case <-time.After(20 * time.Millisecond):
		}
		task, err := registry.GetStatus(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == SubAgentTaskFailed {
			if !strings.Contains(task.Error, "panic") {
				t.Fatalf("task error should mention the panic, got %q", task.Error)
			}
			return
		}
		if task.Status == SubAgentTaskCompleted || task.Status == SubAgentTaskCancelled {
			t.Fatalf("unexpected terminal status %q", task.Status)
		}
	}
}
