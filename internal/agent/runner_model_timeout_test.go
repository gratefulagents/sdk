package agent

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/internal/modeldelta"
)

// blockingModel blocks every call until the (per-attempt) call context is
// cancelled, then returns that context's error. It models a hung provider
// connection — the failure mode that previously froze a run until kill -9.
type blockingModel struct{}

func (blockingModel) GetResponse(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingModel) StreamResponse(ctx context.Context, _ ModelRequest) (*ModelStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingModel) GetRetryAdvice(_ error) *ModelRetryAdvice { return nil }
func (blockingModel) CalculateCost(_ Usage) float64            { return 0 }
func (blockingModel) Provider() string                         { return "blocking" }

// TestRunnerModelCallTimeoutPreventsHang verifies the per-attempt model-call
// timeout bounds a hung provider request so the run fails instead of freezing
// forever (no external cancellation involved).
func TestRunnerModelCallTimeoutPreventsHang(t *testing.T) {
	runner := NewRunnerWithModel(blockingModel{})
	agent := &Agent{Name: "test", Model: "blocking-model"}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := runner.Run(context.Background(), agent, nil, RunConfig{
			MaxTurns:         2,
			ModelCallTimeout: 100 * time.Millisecond,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the run to fail on a hung model call, got nil error")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("run returned but took too long (%v); model idle timeout not effective", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung despite ModelCallTimeout — this is the freeze the fix prevents")
	}
}

type pacedStreamModel struct {
	interval time.Duration
	events   int
}

func (m pacedStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("runner must consume StreamResponse for activity")
}

func (m pacedStreamModel) StreamResponse(ctx context.Context, _ ModelRequest) (*ModelStream, error) {
	events := make(chan ModelStreamEvent)
	done := make(chan *ModelResponse, 1)
	go func() {
		defer close(events)
		for i := 0; i < m.events; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.interval):
			}
			select {
			case <-ctx.Done():
				return
			case events <- ModelStreamEvent{Type: ModelStreamReasoningDelta, Delta: "thinking"}:
			}
		}
		resp := &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}}
		events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
		done <- resp
	}()
	return NewModelStream(events, done), nil
}

func (pacedStreamModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (pacedStreamModel) CalculateCost(Usage) float64            { return 0 }
func (pacedStreamModel) Provider() string                       { return "paced" }

func TestRunnerModelCallTimeoutResetsOnStreamActivity(t *testing.T) {
	const idleTimeout = 150 * time.Millisecond
	model := pacedStreamModel{interval: 30 * time.Millisecond, events: 6}
	started := time.Now()

	streamed := NewRunnerWithModel(model).RunStreamed(context.Background(), &Agent{Name: "test"}, nil, RunConfig{
		MaxTurns:         2,
		ModelCallTimeout: idleTimeout,
	})
	for range streamed.Events {
	}
	result := streamed.FinalResult()
	if err := streamed.Err(); err != nil {
		t.Fatalf("RunStreamed() error = %v", err)
	}
	if result == nil || result.FinalOutput != "done" {
		t.Fatalf("RunStreamed() result = %+v, want final output done", result)
	}
	if elapsed := time.Since(started); elapsed <= idleTimeout {
		t.Fatalf("generation completed in %v; test must exceed the %v idle budget", elapsed, idleTimeout)
	}
}

type authoritativeFinalStreamModel struct{}

func (authoritativeFinalStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("unexpected GetResponse call")
}

func (authoritativeFinalStreamModel) StreamResponse(context.Context, ModelRequest) (*ModelStream, error) {
	events := make(chan ModelStreamEvent, 1)
	done := make(chan *ModelResponse)
	eventResp := &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "event"}}}}
	finalResp := &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "final"}}}}
	events <- ModelStreamEvent{Type: ModelStreamComplete, Response: eventResp}
	close(events)
	go func() { done <- finalResp }()
	return NewModelStream(events, done), nil
}

func (authoritativeFinalStreamModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (authoritativeFinalStreamModel) CalculateCost(Usage) float64            { return 0 }
func (authoritativeFinalStreamModel) Provider() string                       { return "final" }

func TestRunnerPrefersAuthoritativeStreamFinalResponse(t *testing.T) {
	result, err := NewRunnerWithModel(authoritativeFinalStreamModel{}).Run(
		context.Background(), &Agent{Name: "test"}, nil, RunConfig{MaxTurns: 2},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.FinalOutput != "final" {
		t.Fatalf("Run() result = %+v, want authoritative final response", result)
	}
}

func TestRunnerRunResetsTimeoutFromPublicStreamActivity(t *testing.T) {
	const idleTimeout = 150 * time.Millisecond
	model := pacedStreamModel{interval: 30 * time.Millisecond, events: 6}
	started := time.Now()

	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test"}, nil, RunConfig{
		MaxTurns:         2,
		ModelCallTimeout: idleTimeout,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.FinalOutput != "done" {
		t.Fatalf("Run() result = %+v, want final output done", result)
	}
	if elapsed := time.Since(started); elapsed <= idleTimeout {
		t.Fatalf("generation completed in %v; test must exceed the %v idle budget", elapsed, idleTimeout)
	}
}

type silentAfterProgressModel struct{}

func (silentAfterProgressModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("unexpected GetResponse call")
}

func (silentAfterProgressModel) StreamResponse(ctx context.Context, _ ModelRequest) (*ModelStream, error) {
	events := make(chan ModelStreamEvent)
	done := make(chan *ModelResponse)
	go func() {
		defer close(events)
		select {
		case events <- ModelStreamEvent{Type: ModelStreamReasoningDelta, Delta: "started"}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return NewModelStream(events, done), nil
}

func (silentAfterProgressModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (silentAfterProgressModel) CalculateCost(Usage) float64            { return 0 }
func (silentAfterProgressModel) Provider() string                       { return "silent" }

func TestRunnerModelCallTimeoutFailsWhenStreamBecomesSilentAfterProgress(t *testing.T) {
	streamed := NewRunnerWithModel(silentAfterProgressModel{}).RunStreamed(
		context.Background(),
		&Agent{Name: "test"},
		nil,
		RunConfig{MaxTurns: 2, ModelCallTimeout: 50 * time.Millisecond},
	)
	for range streamed.Events {
	}
	if err := streamed.Err(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunStreamed() error = %v, want deadline exceeded after stream silence", err)
	}
}

type committedThenSilentModel struct {
	calls atomic.Int32
}

func (*committedThenSilentModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("unexpected GetResponse call")
}

func (m *committedThenSilentModel) StreamResponse(ctx context.Context, _ ModelRequest) (*ModelStream, error) {
	m.calls.Add(1)
	events := make(chan ModelStreamEvent)
	done := make(chan *ModelResponse)
	go func() {
		defer close(events)
		select {
		case events <- ModelStreamEvent{Type: ModelStreamDelta, Delta: "visible"}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return NewModelStream(events, done), nil
}

func (*committedThenSilentModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (*committedThenSilentModel) CalculateCost(Usage) float64            { return 0 }
func (*committedThenSilentModel) Provider() string                       { return "committed" }

func TestRunnerDoesNotRetryIdleTimeoutAfterOutputCommitted(t *testing.T) {
	model := &committedThenSilentModel{}
	var handlerCalls atomic.Int32
	streamed := NewRunnerWithModel(model).RunStreamed(context.Background(), &Agent{Name: "test"}, nil, RunConfig{
		MaxTurns:         2,
		ModelCallTimeout: 50 * time.Millisecond,
		RetryPolicy:      &RetryPolicy{MaxRetries: 1},
		ErrorHandler: func(RunErrorData) RunErrorHandlerResult {
			handlerCalls.Add(1)
			return RunErrorHandlerResult{Action: ErrorActionRetry}
		},
	})
	for range streamed.Events {
	}
	if err := streamed.Err(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunStreamed() error = %v, want committed-output idle timeout", err)
	}
	if calls := model.calls.Load(); calls != 1 {
		t.Fatalf("model calls = %d, want no retry after visible output", calls)
	}
	if calls := handlerCalls.Load(); calls != 0 {
		t.Fatalf("error handler calls = %d, want committed output to bypass retry decisions", calls)
	}
}

type internalReasoningErrorModel struct {
	calls atomic.Int32
}

func (*internalReasoningErrorModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("unexpected GetResponse call")
}

func (m *internalReasoningErrorModel) StreamResponse(ctx context.Context, _ ModelRequest) (*ModelStream, error) {
	m.calls.Add(1)
	if sink := modeldelta.ReasoningSinkFromContext(ctx); sink != nil {
		sink("visible internal reasoning")
	}
	return nil, errors.New("provider stream failed after reasoning")
}

func (*internalReasoningErrorModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (*internalReasoningErrorModel) CalculateCost(Usage) float64            { return 0 }
func (*internalReasoningErrorModel) Provider() string                       { return "reasoning-error" }

func TestRunnerDoesNotRetryAfterProviderEmitsInternalReasoning(t *testing.T) {
	model := &internalReasoningErrorModel{}
	var events bytes.Buffer
	var handlerCalls atomic.Int32
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test"}, nil, RunConfig{
		MaxTurns:    2,
		Hooks:       NewPlatformHooks(NewProgressTracker(), NewEventStream(&events)),
		RetryPolicy: &RetryPolicy{MaxRetries: 1},
		ErrorHandler: func(RunErrorData) RunErrorHandlerResult {
			handlerCalls.Add(1)
			return RunErrorHandlerResult{Action: ErrorActionRetry}
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want provider failure")
	}
	if calls := model.calls.Load(); calls != 1 {
		t.Fatalf("model calls = %d, want no retry after internal reasoning", calls)
	}
	if calls := handlerCalls.Load(); calls != 0 {
		t.Fatalf("error handler calls = %d, want committed reasoning to bypass retry decisions", calls)
	}
	if !bytes.Contains(events.Bytes(), []byte("visible internal reasoning")) {
		t.Fatal("internal reasoning was not emitted to the event stream")
	}
}

func TestRunnerRetriesAfterModelStreamIdleTimeout(t *testing.T) {
	model := &timeoutOnceSubagentModel{}
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test"}, nil, RunConfig{
		MaxTurns:         2,
		ModelCallTimeout: 30 * time.Millisecond,
		RetryPolicy:      &RetryPolicy{MaxRetries: 1},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.FinalOutput != "recovered" {
		t.Fatalf("Run() result = %+v, want recovered", result)
	}
	model.mu.Lock()
	calls := model.calls
	model.mu.Unlock()
	if calls != 2 {
		t.Fatalf("model calls = %d, want timeout plus one retry", calls)
	}
}

type countingBlockingModel struct {
	calls     atomic.Int32
	started   chan struct{}
	startOnce sync.Once
}

func (m *countingBlockingModel) GetResponse(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	m.calls.Add(1)
	m.startOnce.Do(func() { close(m.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *countingBlockingModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	_, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	return nil, errors.New("unexpected response")
}

func (*countingBlockingModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (*countingBlockingModel) CalculateCost(Usage) float64            { return 0 }
func (*countingBlockingModel) Provider() string                       { return "blocking" }

func TestRunnerReturnsParentCancellationWithoutRetryingModelCall(t *testing.T) {
	model := &countingBlockingModel{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewRunnerWithModel(model).Run(ctx, &Agent{Name: "test"}, nil, RunConfig{
			MaxTurns:         2,
			ModelCallTimeout: time.Second,
			RetryPolicy:      &RetryPolicy{MaxRetries: 3},
		})
		done <- err
	}()

	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model call did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after parent cancellation")
	}
	if calls := model.calls.Load(); calls != 1 {
		t.Fatalf("model calls = %d, want no retry after parent cancellation", calls)
	}
}

func TestModelCallIdleContextPreservesParentCancellation(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	ctx, _, cancel := modelCallIdleContext(parent, time.Second)
	parentCancel()
	<-ctx.Done()
	cancel()
	if errors.Is(context.Cause(ctx), errModelStreamIdleTimeout) {
		t.Fatal("parent cancellation was misclassified as a model idle timeout")
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("context cause = %v, want context.Canceled", context.Cause(ctx))
	}
}

// TestRunnerCompactionModelResolverUsesActiveModel verifies the runner consults
// CompactionModelResolver with the model actually being used, so sub-agents (and
// fallback models) get their own model's thresholds rather than inheriting the
// parent's.
func TestRunnerCompactionModelResolverUsesActiveModel(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
				Usage: Usage{InputTokens: 5, OutputTokens: 2},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Model: "spark-model-x"}

	var mu sync.Mutex
	var seen []string
	cfg := RunConfig{
		MaxTurns: 2,
		// Enabled with a high trigger so no actual compaction occurs; we only
		// assert the resolver is consulted with the active model.
		CompactionConfig: CompactionConfig{Enabled: true, TriggerTokens: 999999, TargetTokens: 100000},
		CompactionModelResolver: func(_ context.Context, m string) (int, int, bool) {
			mu.Lock()
			seen = append(seen, m)
			mu.Unlock()
			return 111111, 55555, true
		},
	}

	if _, err := runner.Run(context.Background(), agent, nil, cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, m := range seen {
		if m == "spark-model-x" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("CompactionModelResolver was not called with the active model; saw %v", seen)
	}
}
