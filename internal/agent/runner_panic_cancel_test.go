package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// panicRunHooks panics on OnAgentStart to verify the runner recovers.
type panicRunHooks struct {
	NoOpRunHooks
	called bool
}

func (h *panicRunHooks) OnAgentStart(_ *RunContext, _ *Agent) {
	h.called = true
	panic("hook boom")
}

// A tool that panics during Execute must not crash the runner.
func TestRunnerRecoversFromPanickingTool(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID: "call1", Name: "boom", Input: json.RawMessage(`{}`),
			}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	boom := &FunctionTool{
		ToolName:        "boom",
		ToolDescription: "panics",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			panic("boom from tool")
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{boom}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (panic should be captured as tool error)", err)
	}
	if result.FinalText() != "done" {
		t.Fatalf("FinalText = %q, want 'done'", result.FinalText())
	}
	var sawErr bool
	for _, item := range result.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.IsError &&
			strings.Contains(strings.ToLower(item.ToolOutput.Content), "panic") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("expected a tool output marked IsError with 'panic' substring, items=%+v", result.NewItems)
	}
}

// A guardrail that panics must surface as a run error rather than crash.
func TestRunnerRecoversFromPanickingInputGuardrail(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name: "test",
		InputGuardrails: []InputGuardrail{{
			Name: "panicker",
			Fn: func(_ *RunContext, _ *Agent, _ []RunItem) (*GuardrailResult, error) {
				panic("kaboom")
			},
		}},
	}

	_, err := runner.Run(context.Background(), agent, []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "hi"}},
	}, RunConfig{})
	if err == nil {
		t.Fatal("Run() error = nil, want guardrail panic surfaced as error")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Run() error = %v, want substring 'panic'", err)
	}
}

// A run hook that panics must not propagate; the run should complete normally.
func TestRunnerRecoversFromPanickingRunHook(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	hooks := &panicRunHooks{}
	agent := &Agent{Name: "test"}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{Hooks: hooks})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (hook panic must be swallowed)", err)
	}
	if result.FinalText() != "ok" {
		t.Fatalf("FinalText = %q, want 'ok'", result.FinalText())
	}
	if !hooks.called {
		t.Fatal("OnAgentStart was never called")
	}
}

// hangingStreamModel blocks in StreamResponse until ctx is cancelled, simulating
// a slow upstream model. The runner must observe ctx cancellation and return.
type hangingStreamModel struct{}

func (hangingStreamModel) GetResponse(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hangingStreamModel) StreamResponse(ctx context.Context, _ ModelRequest) (*ModelStream, error) {
	events := make(chan ModelStreamEvent)
	done := make(chan *ModelResponse, 1)
	go func() {
		defer close(events)
		<-ctx.Done()
		done <- &ModelResponse{}
	}()
	return NewModelStream(events, done), nil
}

func (hangingStreamModel) GetRetryAdvice(_ error) *ModelRetryAdvice { return &ModelRetryAdvice{} }
func (hangingStreamModel) CalculateCost(_ Usage) float64            { return 0 }
func (hangingStreamModel) Provider() string                         { return "hanging" }

func TestRunnerRunStreamedRespectsContextCancellation(t *testing.T) {
	runner := NewRunnerWithModel(hangingStreamModel{})
	agent := &Agent{Name: "test"}

	ctx, cancel := context.WithCancel(context.Background())
	stream := runner.RunStreamed(ctx, agent, nil, RunConfig{})

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = stream.FinalResult()
	}()

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("RunStreamed did not return within 3s of ctx cancellation")
	}

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

func TestRunnerRunStreamedExposesTerminalError(t *testing.T) {
	wantErr := errors.New("stream failure")
	runner := NewRunnerWithModel(&mockModel{errors: []error{wantErr}})
	agent := &Agent{Name: "test"}

	stream := runner.RunStreamed(context.Background(), agent, nil, RunConfig{MaxTurns: 1})
	for range stream.Events {
	}
	result := stream.FinalResult()
	if result == nil {
		t.Fatal("FinalResult() = nil")
	}
	if !errors.Is(stream.Err(), wantErr) {
		t.Fatalf("Err() = %v, want %v", stream.Err(), wantErr)
	}
}

func TestRunnerReturnsErrorForNilAgent(t *testing.T) {
	runner := NewRunnerWithModel(&mockModel{})

	if _, err := runner.Run(context.Background(), nil, nil, RunConfig{}); err == nil || !strings.Contains(err.Error(), "agent is nil") {
		t.Fatalf("Run() error = %v, want nil-agent error", err)
	}
}

func TestRunnerRunStreamedReturnsErrorForNilAgent(t *testing.T) {
	runner := NewRunnerWithModel(&mockModel{})

	stream := runner.RunStreamed(context.Background(), nil, nil, RunConfig{})
	for range stream.Events {
	}
	result := stream.FinalResult()
	if result == nil {
		t.Fatal("FinalResult() = nil")
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "agent is nil") {
		t.Fatalf("Err() = %v, want nil-agent error", err)
	}
}

type streamErrorModel struct {
	err error
}

func (m streamErrorModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("unexpected non-streaming call")
}

func (m streamErrorModel) StreamResponse(context.Context, ModelRequest) (*ModelStream, error) {
	events := make(chan ModelStreamEvent, 1)
	done := make(chan *ModelResponse, 1)
	go func() {
		defer close(events)
		defer close(done)
		events <- ModelStreamEvent{Type: ModelStreamError, Error: m.err}
		done <- nil
	}()
	return NewModelStream(events, done), nil
}

func (m streamErrorModel) GetRetryAdvice(error) *ModelRetryAdvice {
	return &ModelRetryAdvice{ShouldRetry: false}
}

func (m streamErrorModel) CalculateCost(Usage) float64 { return 0 }
func (m streamErrorModel) Provider() string            { return "stream-error" }

func TestRunnerRunStreamedPropagatesModelStreamError(t *testing.T) {
	wantErr := errors.New("provider stream broke")
	runner := NewRunnerWithModel(streamErrorModel{err: wantErr})

	stream := runner.RunStreamed(context.Background(), &Agent{Name: "test"}, nil, RunConfig{MaxTurns: 1})
	for range stream.Events {
	}
	result := stream.FinalResult()
	if result == nil {
		t.Fatal("FinalResult() = nil")
	}
	if !errors.Is(stream.Err(), wantErr) {
		t.Fatalf("Err() = %v, want %v", stream.Err(), wantErr)
	}
}
