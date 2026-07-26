package agent

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

type resumeDispatchProbeModel struct {
	dispatched chan struct{}
}

func (m *resumeDispatchProbeModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	select {
	case m.dispatched <- struct{}{}:
	default:
	}
	return nil, errors.New("model dispatched")
}

func (m *resumeDispatchProbeModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	_, err := m.GetResponse(ctx, req)
	return nil, err
}

func (*resumeDispatchProbeModel) GetRetryAdvice(error) *ModelRetryAdvice {
	return &ModelRetryAdvice{}
}

func (*resumeDispatchProbeModel) CalculateCost(Usage) float64 { return 0 }
func (*resumeDispatchProbeModel) Provider() string            { return "probe" }

func TestSubAgentSpawnFailsClosedWhenCheckpointFails(t *testing.T) {
	failure := errors.New("store unavailable")
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Agents:     map[string]*Agent{"worker": {Name: "worker"}},
		Checkpoint: func(SubAgentSchedulerCheckpoint) error { return failure },
	})
	id, err := registry.SpawnAsync(context.Background(), "worker", "work", ToolAccessLevelFull)
	if id != "" || !errors.Is(err, failure) {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if len(registry.ListTasks()) != 0 {
		t.Fatal("child launched without durable pending checkpoint")
	}
}

func TestFailedSpawnDoesNotExposeProvisionalSnapshot(t *testing.T) {
	failure := errors.New("store unavailable")
	entered := make(chan struct{})
	release := make(chan struct{})
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
		Checkpoint: func(SubAgentSchedulerCheckpoint) error {
			close(entered)
			<-release
			return failure
		},
	})
	spawned := make(chan error, 1)
	go func() {
		_, err := registry.SpawnAsync(context.Background(), "worker", "work", ToolAccessLevelFull)
		spawned <- err
	}()
	<-entered
	snapshotStarted := make(chan struct{})
	snapshotDone := make(chan SubAgentSchedulerCheckpoint, 1)
	go func() {
		close(snapshotStarted)
		snapshotDone <- registry.SchedulerCheckpoint()
	}()
	<-snapshotStarted
	runtime.Gosched()
	if snapshot := <-snapshotDone; len(snapshot.Records) != 0 {
		t.Fatalf("provisional snapshot escaped: %+v", snapshot)
	}
	close(release)
	if err := <-spawned; !errors.Is(err, failure) {
		t.Fatalf("spawn error = %v", err)
	}
}

func TestRestoredActiveChildRequiresExplicitReconciliation(t *testing.T) {
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{Task: SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"}}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	task, err := registry.GetStatus("child-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskReconciling {
		t.Fatalf("status=%q", task.Status)
	}
	if err := registry.ReconcileRestoredTask("child-1", SubAgentTaskCancelled, "", "operator cancelled uncertain child"); err != nil {
		t.Fatal(err)
	}
	task, _ = registry.GetStatus("child-1")
	if task.Status != SubAgentTaskCancelled {
		t.Fatalf("status=%q", task.Status)
	}
}

func TestChildRunnerCheckpointIsPersistedWithSecurityBaseline(t *testing.T) {
	var latestMu sync.Mutex
	var latest SubAgentSchedulerCheckpoint
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: NewRunnerWithModel(&mockModel{responses: []*ModelResponse{{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
		}}}),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
		Checkpoint: func(checkpoint SubAgentSchedulerCheckpoint) error {
			latestMu.Lock()
			latest = checkpoint
			latestMu.Unlock()
			return nil
		},
	})
	id, err := registry.SpawnAsync(context.Background(), "worker", "work", ToolAccessLevelReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.WaitForTask(context.Background(), id, 1000); err != nil {
		t.Fatal(err)
	}
	latestMu.Lock()
	record := latest.Records[0]
	latestMu.Unlock()
	if record.DurableCheckpoint == nil || record.DurableCheckpoint.Boundary != DurableBoundaryRunCompleted {
		t.Fatalf("child durable checkpoint = %+v", record.DurableCheckpoint)
	}
	if record.SecurityBaseline == nil || record.SecurityBaseline.ToolAccessLevel != ToolAccessLevelReadOnly {
		t.Fatalf("security baseline = %+v", record.SecurityBaseline)
	}
}

func TestRestoreRequeuesInFlightSteeringBeforeQueued(t *testing.T) {
	inFlight := RunItem{Type: RunItemMessage, Message: &MessageOutput{Text: "[PARENT MESSAGE]\nin-flight"}}
	queued := RunItem{Type: RunItemMessage, Message: &MessageOutput{Text: "[PARENT MESSAGE]\nqueued"}}
	original := DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		Boundary:      DurableBoundaryRunStarted,
		History:       SnapshotRunItems([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "original"}}}),
	}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task:              SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"},
		DurableCheckpoint: &original,
		SecurityBaseline:  securityBaseline(ToolAccessLevelFull, nil, nil, nil, nil, 0),
		QueuedMessages:    SnapshotRunItems([]RunItem{queued}),
		InFlightMessages:  SnapshotRunItems([]RunItem{inFlight}),
	}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	original.History[0].MessageText = "mutated"
	restored := registry.SchedulerCheckpoint().Records[0]
	if restored.DurableCheckpoint.History[0].MessageText != "original" {
		t.Fatalf("durable checkpoint was not cloned: %+v", restored.DurableCheckpoint.History)
	}
	messages := registry.tasks["child-1"].queuedMessages
	if len(messages) != 2 || messages[0].Message.Text != inFlight.Message.Text || messages[1].Message.Text != queued.Message.Text {
		t.Fatalf("restored steering order = %+v", messages)
	}
}

func TestResumeRestoredCompletedChildDoesNotDispatch(t *testing.T) {
	var persisted []SubAgentSchedulerCheckpoint
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{Checkpoint: func(checkpoint SubAgentSchedulerCheckpoint) error {
		persisted = append(persisted, checkpoint)
		return nil
	}})
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task: SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"},
		DurableCheckpoint: &DurableCheckpoint{
			SchemaVersion: DurableCheckpointSchemaVersion,
			Boundary:      DurableBoundaryRunCompleted,
			History:       SnapshotRunItems([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}),
		},
		SecurityBaseline: securityBaseline(ToolAccessLevelFull, nil, nil, nil, nil, 0),
	}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := registry.ResumeRestoredTask(context.Background(), "child-1"); err != nil {
		t.Fatal(err)
	}
	task, err := registry.GetStatus("child-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCompleted || task.Result != "done" {
		t.Fatalf("completed resume = %+v", task)
	}
	if len(persisted) < 2 || persisted[len(persisted)-1].Records[0].Task.Status != SubAgentTaskCompleted {
		t.Fatalf("completed transition was not persisted: %+v", persisted)
	}
}

func TestCheckpointHookCanReenterSchedulerCheckpoint(t *testing.T) {
	failure := errors.New("store unavailable")
	var registry *SubAgentRegistry
	var checkpoint, reentered SubAgentSchedulerCheckpoint
	registry = NewSubAgentRegistry(SubAgentRegistryConfig{
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
		Checkpoint: func(snapshot SubAgentSchedulerCheckpoint) error {
			checkpoint = snapshot
			reentered = registry.SchedulerCheckpoint()
			return failure
		},
	})
	if _, err := registry.SpawnAsync(context.Background(), "worker", "work", ToolAccessLevelFull); !errors.Is(err, failure) {
		t.Fatalf("spawn error = %v", err)
	}
	if len(checkpoint.Records) != 1 || checkpoint.Records[0].Task.Status != SubAgentTaskPending {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	if len(reentered.Records) != 0 {
		t.Fatalf("reentered snapshot exposed provisional task: %+v", reentered)
	}
}

func TestReconcileRestoredTaskPersistsTerminalDecision(t *testing.T) {
	var persisted []SubAgentSchedulerCheckpoint
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{Checkpoint: func(snapshot SubAgentSchedulerCheckpoint) error {
		persisted = append(persisted, snapshot)
		return nil
	}})
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task: SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"},
	}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	persisted = nil
	if err := registry.ReconcileRestoredTask("child-1", SubAgentTaskCompleted, "done", ""); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Records[0].Task.Status != SubAgentTaskCompleted || persisted[0].Records[0].Task.Result != "done" {
		t.Fatalf("terminal decision was not persisted: %+v", persisted)
	}
}

func TestReconcileRestoredTaskRollsBackWhenCheckpointFails(t *testing.T) {
	failure := errors.New("store unavailable")
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task: SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"},
	}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	registry.Configure(SubAgentRegistryConfig{Checkpoint: func(SubAgentSchedulerCheckpoint) error { return failure }})
	if err := registry.ReconcileRestoredTask("child-1", SubAgentTaskCancelled, "", "cancelled"); !errors.Is(err, failure) {
		t.Fatalf("reconcile error = %v", err)
	}
	task, err := registry.GetStatus("child-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskReconciling || task.Error != subAgentRuntimeRestartError {
		t.Fatalf("failed reconciliation was not rolled back: %+v", task)
	}
}

func TestResumeRestoredTaskCancellationDuringPersistenceDoesNotDispatch(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	model := &resumeDispatchProbeModel{dispatched: make(chan struct{}, 1)}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task: SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"},
		DurableCheckpoint: &DurableCheckpoint{
			SchemaVersion: DurableCheckpointSchemaVersion,
			Boundary:      DurableBoundaryRunStarted,
		},
		SecurityBaseline: securityBaseline(ToolAccessLevelFull, nil, nil, nil, nil, 0),
	}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	registry.Configure(SubAgentRegistryConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
		Checkpoint: func(snapshot SubAgentSchedulerCheckpoint) error {
			if len(snapshot.Records) == 1 && snapshot.Records[0].Task.Status == SubAgentTaskPending {
				close(entered)
				<-release
			}
			return nil
		},
	})
	resumed := make(chan error, 1)
	go func() { resumed <- registry.ResumeRestoredTask(context.Background(), "child-1") }()
	<-entered
	cancelled := make(chan error, 1)
	go func() { cancelled <- registry.Cancel("child-1") }()
	deadline := time.After(time.Second)
	for {
		task, err := registry.GetStatus("child-1")
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == SubAgentTaskCancelled {
			break
		}
		select {
		case <-deadline:
			t.Fatal("cancellation did not win while resume checkpoint was blocked")
		default:
			runtime.Gosched()
		}
	}
	close(release)
	if err := <-resumed; err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if err := <-cancelled; err != nil {
		t.Fatalf("cancel error = %v", err)
	}
	task, err := registry.GetStatus("child-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCancelled {
		t.Fatalf("status = %q", task.Status)
	}
	select {
	case <-model.dispatched:
		t.Fatal("model dispatched after cancellation")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResumeRestoredTaskRejectsUnreconciledOrWeakerSecurity(t *testing.T) {
	for _, test := range []struct {
		name     string
		boundary DurableBoundary
		policy   *ToolPolicy
	}{
		{name: "unreconciled model", boundary: DurableBoundaryModelCompleted, policy: &ToolPolicy{ApprovalRequired: true}},
		{name: "weaker policy", boundary: DurableBoundaryRunStarted, policy: &ToolPolicy{ApprovalRequired: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := NewSubAgentRegistry(SubAgentRegistryConfig{Checkpoint: func(SubAgentSchedulerCheckpoint) error { return nil }})
			checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
				Task:              SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"},
				DurableCheckpoint: &DurableCheckpoint{SchemaVersion: DurableCheckpointSchemaVersion, Boundary: test.boundary},
				SecurityBaseline:  securityBaseline(ToolAccessLevelFull, test.policy, nil, nil, nil, 0),
			}}}
			if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
				t.Fatal(err)
			}
			if err := registry.ResumeRestoredTask(context.Background(), "child-1"); err == nil {
				t.Fatal("expected resume rejection")
			}
			task, _ := registry.GetStatus("child-1")
			if task.Status != SubAgentTaskReconciling {
				t.Fatalf("failed resume changed task status: %+v", task)
			}
		})
	}
}
