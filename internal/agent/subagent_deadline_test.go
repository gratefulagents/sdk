package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

type timeoutOnceSubagentModel struct {
	mu    sync.Mutex
	calls int
}

func (m *timeoutOnceSubagentModel) GetResponse(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	if call == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "recovered"}}}}, nil
}

func (m *timeoutOnceSubagentModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	resp, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan ModelStreamEvent, 1)
	done := make(chan *ModelResponse, 1)
	events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
	close(events)
	done <- resp
	return NewModelStream(events, done), nil
}

func (*timeoutOnceSubagentModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (*timeoutOnceSubagentModel) CalculateCost(Usage) float64            { return 0 }
func (*timeoutOnceSubagentModel) Provider() string                       { return "test" }

func TestAsyncSubagentInheritsParentModelRetryAndTimeout(t *testing.T) {
	model := &timeoutOnceSubagentModel{}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	parentCfg := RunConfig{
		RetryPolicy:      &RetryPolicy{MaxRetries: 1},
		ModelCallTimeout: 20 * time.Millisecond,
	}
	ctx := WithNestedRunConfig(context.Background(), parentCfg)
	taskID, err := registry.SpawnAsync(ctx, "worker", "recover from a transient timeout", ToolAccessLevelReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	task, err := registry.WaitForTask(context.Background(), taskID, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCompleted || task.Result != "recovered" {
		t.Fatalf("task = %+v, want completed retry result", task)
	}
	model.mu.Lock()
	calls := model.calls
	model.mu.Unlock()
	if calls != 2 {
		t.Fatalf("model calls = %d, want timeout plus one inherited retry", calls)
	}
}
