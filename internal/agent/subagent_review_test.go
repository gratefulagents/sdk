package agent

import (
	"context"
	"testing"
)

func TestResumeRestoredTaskWithoutRunnerCheckpointRelaunchesSameID(t *testing.T) {
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: NewRunnerWithModel(&mockModel{responses: []*ModelResponse{{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "recovered"}}},
		}}}),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
		Checkpoint: func(SubAgentSchedulerCheckpoint) error {
			return nil
		},
	})
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task: SubAgentTask{
			ID:        "child-before-run",
			AgentName: "worker",
			Message:   "work",
			Status:    SubAgentTaskPending,
		},
		SecurityBaseline: securityBaseline(ToolAccessLevelFull, nil, nil, nil, nil, 0),
	}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := registry.ResumeRestoredTask(context.Background(), "child-before-run"); err != nil {
		t.Fatal(err)
	}
	task, err := registry.WaitForTask(context.Background(), "child-before-run", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCompleted || task.Result != "recovered" {
		t.Fatalf("resumed task = %+v", task)
	}
}

func TestDurableResumePreservesPriorUsage(t *testing.T) {
	prior := Usage{InputTokens: 10, OutputTokens: 4, CacheReadTokens: 3}
	model := &mockModel{responses: []*ModelResponse{{
		Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
		Usage: Usage{InputTokens: 5, OutputTokens: 2, CacheReadTokens: 1},
	}}}
	runner := NewRunnerWithModel(model)
	result, err := runner.Run(context.Background(), &Agent{Name: "worker"}, nil, RunConfig{
		Durable: &DurableRunConfig{Resume: &DurableCheckpoint{
			SchemaVersion: DurableCheckpointSchemaVersion,
			Boundary:      DurableBoundaryModelPrepared,
			AgentName:     "worker",
			Usage:         prior,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.InputTokens != 15 || result.Usage.OutputTokens != 6 || result.Usage.CacheReadTokens != 4 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}
