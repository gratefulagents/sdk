package agent

import (
	"context"
	"sync"
	"testing"
	"time"
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
			t.Fatalf("run returned but took too long (%v); per-call timeout not effective", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung despite ModelCallTimeout — this is the freeze the fix prevents")
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
