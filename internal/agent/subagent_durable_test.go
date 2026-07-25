package agent

import (
	"context"
	"errors"
	"testing"
)

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
