package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeSchedulerTool stands in for the managed sub-agent tool: it exposes a
// scheduler checkpoint and records restores so the runner's durable wiring can
// be exercised without a model.
type fakeSchedulerTool struct {
	hasTasks   bool
	restored   []SubAgentSchedulerCheckpoint
	restoreErr error
}

func (*fakeSchedulerTool) Name() string                 { return "subagent" }
func (*fakeSchedulerTool) Description() string          { return "fake" }
func (*fakeSchedulerTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*fakeSchedulerTool) IsReadOnly() bool             { return false }
func (*fakeSchedulerTool) IsEnabled(*RunContext) bool   { return true }
func (*fakeSchedulerTool) NeedsApproval() bool          { return false }
func (*fakeSchedulerTool) TimeoutSeconds() int          { return 0 }
func (*fakeSchedulerTool) Execute(context.Context, json.RawMessage, string) (ToolResult, error) {
	return ToolResult{}, nil
}

func (t *fakeSchedulerTool) SubAgentSchedulerCheckpoint() SubAgentSchedulerCheckpoint {
	return SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{Task: SubAgentTask{ID: "live"}}}}
}

func (t *fakeSchedulerTool) RestoreSubAgentSchedulerCheckpoint(cp SubAgentSchedulerCheckpoint) error {
	if t.restoreErr != nil {
		return t.restoreErr
	}
	t.restored = append(t.restored, cp)
	t.hasTasks = true
	return nil
}

func (t *fakeSchedulerTool) HasSubAgentTasks() bool { return t.hasTasks }

func childRecords(statuses ...SubAgentTaskStatus) *SubAgentSchedulerCheckpoint {
	cp := &SubAgentSchedulerCheckpoint{}
	for i, status := range statuses {
		cp.Records = append(cp.Records, SubAgentSchedulerCheckpointRecord{
			Task: SubAgentTask{ID: "child-" + string(rune('a'+i)), AgentName: "worker", Status: status},
		})
	}
	return cp
}

func TestWireDurableChildrenAutoWiresCheckpointCallback(t *testing.T) {
	tool := &fakeSchedulerTool{}
	cfg := &DurableRunConfig{}
	if err := wireDurableChildren(cfg, []Tool{tool}); err != nil {
		t.Fatal(err)
	}
	if cfg.Children == nil {
		t.Fatal("expected Children callback to be wired from the scheduler tool")
	}
	if got := cfg.Children(); len(got.Records) != 1 || got.Records[0].Task.ID != "live" {
		t.Fatalf("Children callback did not read the scheduler: %+v", got)
	}
}

func TestWireDurableChildrenKeepsHostProvidedCallback(t *testing.T) {
	hostCalls := 0
	cfg := &DurableRunConfig{Children: func() SubAgentSchedulerCheckpoint {
		hostCalls++
		return SubAgentSchedulerCheckpoint{}
	}}
	if err := wireDurableChildren(cfg, []Tool{&fakeSchedulerTool{}}); err != nil {
		t.Fatal(err)
	}
	cfg.Children()
	if hostCalls != 1 {
		t.Fatal("host-provided Children callback was replaced")
	}
}

func TestWireDurableChildrenRestoresIntoEmptyScheduler(t *testing.T) {
	tool := &fakeSchedulerTool{}
	cfg := &DurableRunConfig{Resume: &DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		Children:      childRecords(SubAgentTaskRunning, SubAgentTaskCompleted),
	}}
	if err := wireDurableChildren(cfg, []Tool{tool}); err != nil {
		t.Fatal(err)
	}
	if len(tool.restored) != 1 || len(tool.restored[0].Records) != 2 {
		t.Fatalf("expected one restore with both records, got %+v", tool.restored)
	}
}

func TestWireDurableChildrenSkipsRestoreWhenHostAlreadyRestored(t *testing.T) {
	tool := &fakeSchedulerTool{hasTasks: true}
	cfg := &DurableRunConfig{Resume: &DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		Children:      childRecords(SubAgentTaskRunning),
	}}
	if err := wireDurableChildren(cfg, []Tool{tool}); err != nil {
		t.Fatal(err)
	}
	if len(tool.restored) != 0 {
		t.Fatal("restore must not run into a non-empty scheduler")
	}
}

func TestWireDurableChildrenFailsClosedForActiveRecordsWithoutScheduler(t *testing.T) {
	cfg := &DurableRunConfig{Resume: &DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		Children:      childRecords(SubAgentTaskCompleted, SubAgentTaskRunning),
	}}
	err := wireDurableChildren(cfg, []Tool{disabledTestTool{}})
	if err == nil || !strings.Contains(err.Error(), "1 active child task record") {
		t.Fatalf("expected active-record error, got %v", err)
	}
}

func TestWireDurableChildrenIgnoresTerminalRecordsWithoutScheduler(t *testing.T) {
	cfg := &DurableRunConfig{Resume: &DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		Children:      childRecords(SubAgentTaskCompleted, SubAgentTaskFailed),
	}}
	if err := wireDurableChildren(cfg, nil); err != nil {
		t.Fatalf("terminal-only records must not block resume: %v", err)
	}
}

func TestWireDurableChildrenPropagatesRestoreError(t *testing.T) {
	boom := errors.New("boom")
	tool := &fakeSchedulerTool{restoreErr: boom}
	cfg := &DurableRunConfig{Resume: &DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		Children:      childRecords(SubAgentTaskRunning),
	}}
	if err := wireDurableChildren(cfg, []Tool{tool}); !errors.Is(err, boom) {
		t.Fatalf("expected wrapped restore error, got %v", err)
	}
}

// End-to-end: a real registry behind the real subagent tool surface is
// restored by Runner.Run from a parent checkpoint that carries child records.
func TestRunnerRunRestoresChildRecordsFromParentCheckpoint(t *testing.T) {
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{Checkpoint: func(SubAgentSchedulerCheckpoint) error { return nil }})
	tool := &registryCheckpointTool{registry: registry}
	agent := &Agent{Name: "parent", Tools: []Tool{tool}}
	runner := NewRunnerWithModel(&resumeDispatchProbeModel{dispatched: make(chan struct{}, 1)})
	resume := &DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		Boundary:      DurableBoundaryRunCompleted,
		AgentName:     "parent",
		History:       SnapshotRunItems([]RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}),
		Children: &SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
			Task:             SubAgentTask{ID: "child-1", AgentName: "worker", Status: SubAgentTaskRunning, Message: "work"},
			SecurityBaseline: securityBaseline(ToolAccessLevelFull, nil, nil, nil, nil, 0),
		}}},
	}
	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{Durable: &DurableRunConfig{Resume: resume}}); err != nil {
		t.Fatal(err)
	}
	task, err := registry.GetStatus("child-1")
	if err != nil {
		t.Fatalf("child record was not restored: %v", err)
	}
	if task.Status != SubAgentTaskReconciling {
		t.Fatalf("restored active child must be reconciling, got %q", task.Status)
	}
}

// registryCheckpointTool mirrors the exported subagent tool's durable wiring
// on top of a real registry (the exported tool lives in pkg/agentsdk).
type registryCheckpointTool struct {
	fakeSchedulerTool
	registry *SubAgentRegistry
}

func (t *registryCheckpointTool) SubAgentSchedulerCheckpoint() SubAgentSchedulerCheckpoint {
	return t.registry.SchedulerCheckpoint()
}

func (t *registryCheckpointTool) RestoreSubAgentSchedulerCheckpoint(cp SubAgentSchedulerCheckpoint) error {
	return t.registry.RestoreSchedulerCheckpoint(cp)
}

func (t *registryCheckpointTool) HasSubAgentTasks() bool { return t.registry.HasTasks() }
