package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --- H4: spawn_subagent_task doesn't clamp child access ---

func TestSubAgentRegistry_H4_ClampsChildAccessToParentReadOnly(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
	}}
	runner := NewRunnerWithModel(model)
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner:          runner,
		WorkDir:         "/tmp/wd",
		ToolAccessLevel: ToolAccessLevelReadOnly, // parent is read-only
		Agents: map[string]*Agent{
			"analyst": {Name: "analyst"},
		},
	})

	// Caller tries to escalate child to "full" — must be clamped to read-only.
	taskID, err := registry.SpawnAsync(context.Background(), "analyst", "go", ToolAccessLevelFull)
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminalTask(t, registry, taskID)

	if len(model.requests) == 0 {
		t.Fatal("expected at least one model request")
	}
	// We need to verify clamping happened. Check that the registry observes it
	// via the run config the runner saw — easier: check the request's tool list
	// would be filtered (no tools here, but at minimum the workspace context
	// instruction generated for the child should reflect read-only).
	gotInstructions := model.requests[0].Instructions
	if !strings.Contains(strings.ToLower(gotInstructions), "read-only") {
		t.Fatalf("expected child instructions to reflect clamped read-only access, got: %q", gotInstructions)
	}
}

func TestSubAgentRegistry_H4_AllowsDowngradeOverride(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
	}}
	runner := NewRunnerWithModel(model)
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner:          runner,
		WorkDir:         "/tmp/wd",
		ToolAccessLevel: ToolAccessLevelFull, // parent is full
		Agents: map[string]*Agent{
			"explorer": {Name: "explorer"},
		},
	})

	// Downgrading from full → read-only is fine.
	taskID, err := registry.SpawnAsync(context.Background(), "explorer", "go", ToolAccessLevelReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminalTask(t, registry, taskID)
	if len(model.requests) == 0 {
		t.Fatal("expected request")
	}
	if !strings.Contains(strings.ToLower(model.requests[0].Instructions), "read-only") {
		t.Fatalf("expected read-only context, got: %q", model.requests[0].Instructions)
	}
}

// --- M2: sub-agent semaphore matched by name prefix ---

func TestRunner_M2_SemaphoreMatchesAgentToolByType(t *testing.T) {
	// A regular function tool whose name happens to start with "agent_" must NOT
	// be treated as a sub-agent for semaphore purposes.
	tool := &FunctionTool{
		ToolName: "agent_disguised_tool",
		Schema:   json.RawMessage(`{}`),
		Fn:       func(_ context.Context, _ json.RawMessage) (string, error) { return "ok", nil },
	}
	if isSubAgentTool(tool) {
		t.Fatalf("regular FunctionTool with agent_* name must not be classified as sub-agent")
	}
	// Real agentTool (Agent.AsTool) must be detected by type.
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
	}}
	runner := NewRunnerWithModel(model)
	helper := &Agent{Name: "helper"}
	at := helper.AsTool(runner)
	if !isSubAgentTool(at) {
		t.Fatalf("agentTool must be classified as sub-agent")
	}
}
