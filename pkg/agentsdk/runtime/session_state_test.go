package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// blockingModel parks every model call until the context is cancelled, so a
// spawned child stays "running" until the session tears it down.
type blockingModel struct {
	started chan struct{}
	once    sync.Once
}

func (m *blockingModel) GetResponse(ctx context.Context, _ agentsdk.ModelRequest) (*agentsdk.ModelResponse, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingModel) StreamResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelStream, error) {
	_, err := m.GetResponse(ctx, req)
	return nil, err
}

func (*blockingModel) GetRetryAdvice(error) *agentsdk.ModelRetryAdvice { return nil }
func (*blockingModel) CalculateCost(agentsdk.Usage) float64            { return 0 }
func (*blockingModel) Provider() string                                { return "blocking" }

func TestSessionStateCloseCancelsActiveSubAgentTasks(t *testing.T) {
	model := &blockingModel{started: make(chan struct{})}
	flushed := 0
	state := NewSessionState()
	scheduler := state.configureSubAgentScheduler(agentsdk.SubAgentSchedulerConfig{
		Runner: agentsdk.NewRunnerWithModel(model),
		Agents: map[string]*agentsdk.Agent{"slow": {Name: "slow", Model: "slow"}},
		Checkpoint: func(agentsdk.SubAgentSchedulerCheckpoint) error {
			flushed++
			return nil
		},
	})
	taskID, err := scheduler.SpawnAsync(context.Background(), "slow", "block forever", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child never reached the model")
	}

	if err := state.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task, err := scheduler.WaitForTask(ctx, taskID, 0)
	if err != nil {
		t.Fatalf("task did not reach a terminal state after Close: %v", err)
	}
	if task.Status != agentsdk.SubAgentTaskCancelled {
		t.Fatalf("status = %q, want cancelled", task.Status)
	}
	if flushed == 0 {
		t.Fatal("Close must flush the scheduler checkpoint")
	}
	if scheduler.HasActiveTasks() {
		t.Fatal("active tasks remain after Close")
	}
	// Idempotent and nil-safe.
	if err := state.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	var nilState *SessionState
	if err := nilState.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestBuilderOwnedSessionStateIsRegisteredAsCloser(t *testing.T) {
	cfg := Config{
		Provider:           "openai",
		Model:              "gpt-test",
		APIKey:             "sk-test",
		WorkDir:            t.TempDir(),
		EnableTools:        true,
		EnableSubAgents:    true,
		SubAgentMaxTurns:   2,
		ToolAccess:         agentsdk.ToolAccessLevelFull,
		DisableSignalTools: true,
	}
	bundle, err := NewBuilder(cfg).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, closer := range bundle.Closers {
		if closer == bundle.SessionState {
			found = true
		}
	}
	if !found {
		t.Fatal("runtime-owned SessionState must be closed with the bundle")
	}

	cfg.SessionState = NewSessionState()
	hosted, err := NewBuilder(cfg).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, closer := range hosted.Closers {
		if closer == hosted.SessionState {
			t.Fatal("host-supplied SessionState must not be closed by the bundle")
		}
	}
}
