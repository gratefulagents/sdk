package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe wrapper around bytes.Buffer for tests that
// concurrently write to and read from an event-stream sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestBuildWorkspaceContext_Full(t *testing.T) {
	ctx := BuildWorkspaceContext("/workspace/repo", ToolAccessLevelFull)
	if !strings.Contains(ctx, "/workspace/repo") {
		t.Error("should contain workDir")
	}
	if !strings.Contains(ctx, "full (read + write + shell)") {
		t.Error("should describe full access")
	}
	if !strings.Contains(ctx, "relative paths") {
		t.Error("should mention relative paths")
	}
}

func TestBuildWorkspaceContext_ReadOnly(t *testing.T) {
	ctx := BuildWorkspaceContext("/workspace/repo", ToolAccessLevelReadOnly)
	if !strings.Contains(ctx, "read-only") {
		t.Error("should describe read-only access")
	}
}

func TestBuildSubAgentBudgetContext(t *testing.T) {
	ctx := BuildSubAgentBudgetContext(17)
	for _, want := range []string{
		"Turn budget: 17 LLM turns",
		"not one tool call",
		"hard ceiling, not a target",
		"partial summary",
		"Act, don't announce",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("expected budget context to contain %q, got %s", want, ctx)
		}
	}
}

func TestSubAgentActivity_RecordToolEnd_TracksWrites(t *testing.T) {
	a := NewSubAgentActivity()

	a.RecordToolEnd("Write", "internal/agent/runner.go", false, 100)
	a.RecordToolEnd("Edit", "cmd/agent/plan.go", false, 50)
	a.RecordToolEnd("Edit", "cmd/agent/plan.go", false, 30)        // duplicate
	a.RecordToolEnd("Write", "internal/agent/runner.go", true, 10) // error — not tracked

	snap := a.Snapshot(false)

	if len(snap.FilesWritten) != 2 {
		t.Fatalf("expected 2 files written, got %d", len(snap.FilesWritten))
	}
	if snap.FilesWritten[0] != "internal/agent/runner.go" {
		t.Errorf("expected first file written to be runner.go, got %s", snap.FilesWritten[0])
	}
	if snap.FilesWritten[1] != "cmd/agent/plan.go" {
		t.Errorf("expected second file written to be plan.go, got %s", snap.FilesWritten[1])
	}
}

func TestSubAgentActivity_StepInference(t *testing.T) {
	tests := []struct {
		tool  string
		input string
		want  string
	}{
		{"LSP", "hover", "exploring"},
		{"Write", "file.go", "implementing"},
		{"Edit", "file.go", "implementing"},
		{"Bash", "git commit -m fix", "committing"},
		{"Bash", "git add .", "committing"},
		{"Bash", "git diff HEAD", "reviewing"},
	}

	for _, tt := range tests {
		t.Run(tt.tool+"_"+tt.input, func(t *testing.T) {
			a := NewSubAgentActivity()
			a.RecordToolStart(tt.tool, tt.input)
			snap := a.Snapshot(false)
			if snap.CurrentStep != tt.want {
				t.Errorf("tool=%s input=%s: got step %q, want %q", tt.tool, tt.input, snap.CurrentStep, tt.want)
			}
		})
	}
}

func TestSubAgentActivity_CurrentTool(t *testing.T) {
	a := NewSubAgentActivity()

	a.RecordToolStart("Bash", "cat file.go")
	snap := a.Snapshot(false)
	if snap.CurrentTool != "Bash" {
		t.Errorf("expected current tool Bash, got %s", snap.CurrentTool)
	}
	if snap.CurrentInput != "cat file.go" {
		t.Errorf("expected current input 'cat file.go', got %s", snap.CurrentInput)
	}

	a.RecordToolEnd("Bash", "cat file.go", false, 10)
	snap = a.Snapshot(false)
	if snap.CurrentTool != "" {
		t.Errorf("expected current tool empty after end, got %s", snap.CurrentTool)
	}
}

func TestSubAgentActivity_RecentToolsRingBuffer(t *testing.T) {
	a := NewSubAgentActivity()

	// Fill beyond ring buffer capacity.
	for i := 0; i < maxRecentActivityEntries+5; i++ {
		a.RecordToolEnd("Bash", "cat file.go", false, int64(i))
	}

	snap := a.Snapshot(true)
	if len(snap.RecentTools) != maxRecentActivityEntries {
		t.Fatalf("expected ring buffer capped at %d, got %d", maxRecentActivityEntries, len(snap.RecentTools))
	}

	// Oldest entries should have been evicted; first entry should have durationMS=5.
	if snap.RecentTools[0].DurationMS != 5 {
		t.Errorf("expected first entry durationMS=5 (oldest evicted), got %d", snap.RecentTools[0].DurationMS)
	}
}

func TestSubAgentActivity_SnapshotIncludeRecent(t *testing.T) {
	a := NewSubAgentActivity()
	a.RecordToolEnd("Bash", "cat file.go", false, 10)

	snap := a.Snapshot(false)
	if snap.RecentTools != nil {
		t.Error("expected nil recent tools when includeRecent=false")
	}

	snap = a.Snapshot(true)
	if len(snap.RecentTools) != 1 {
		t.Errorf("expected 1 recent tool, got %d", len(snap.RecentTools))
	}
}

func TestSubAgentActivity_BriefStatus(t *testing.T) {
	a := NewSubAgentActivity()

	// No activity yet.
	step, tool, written := a.BriefStatus()
	if step != "" || tool != "" || written != 0 {
		t.Error("expected empty brief status initially")
	}

	// Active tool.
	a.RecordToolStart("Edit", "file.go")
	step, tool, written = a.BriefStatus()
	if step != "implementing" {
		t.Errorf("expected step implementing, got %s", step)
	}
	if tool != "Edit" {
		t.Errorf("expected last tool Edit, got %s", tool)
	}

	// After tool completes.
	a.RecordToolEnd("Edit", "file.go", false, 10)
	step, tool, written = a.BriefStatus()
	if tool != "Edit" {
		t.Errorf("expected last tool Edit from recent, got %s", tool)
	}
	if written != 1 {
		t.Errorf("expected 1 file written, got %d", written)
	}
}

func TestSubAgentActivity_ConcurrentAccess(t *testing.T) {
	a := NewSubAgentActivity()
	var wg sync.WaitGroup
	const n = 100

	// Concurrent writers.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.RecordToolStart("Bash", "cat file.go")
			a.RecordToolEnd("Bash", "cat file.go", false, int64(i))
			a.RecordToolStart("Write", "out.go")
			a.RecordToolEnd("Write", "out.go", false, int64(i))
		}(i)
	}

	// Concurrent readers.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Snapshot(true)
			a.BriefStatus()
		}()
	}

	wg.Wait()

	// Verify no panic and data is consistent.
	snap := a.Snapshot(true)
	if len(snap.FilesWritten) != 1 || snap.FilesWritten[0] != "out.go" {
		t.Errorf("unexpected files written: %v", snap.FilesWritten)
	}
}

func TestSubAgentRegistryConfigurePreservesTrackedTasks(t *testing.T) {
	r := NewSubAgentRegistry(SubAgentRegistryConfig{
		Agents:          map[string]*Agent{"agent": {Name: "agent"}},
		WorkDir:         "/old",
		ToolAccessLevel: ToolAccessLevelFull,
		MaxTurns:        2,
	})
	r.tasks["task_1"] = &subAgentTaskEntry{task: SubAgentTask{ID: "task_1", AgentName: "agent", Status: SubAgentTaskRunning}}
	r.order = []string{"task_1"}
	changed := r.changed

	r.Configure(SubAgentRegistryConfig{
		Agents:          map[string]*Agent{"reviewer": {Name: "reviewer"}},
		WorkDir:         "/new",
		ToolAccessLevel: ToolAccessLevelReadOnly,
		MaxConcurrent:   3,
		MaxTurns:        5,
	})

	if len(r.ListTasks()) != 1 {
		t.Fatalf("tracked tasks were not preserved")
	}
	if r.changed != changed {
		t.Fatal("change channel should be preserved for existing waiters")
	}
	if r.workDir != "/new" || r.toolAccessLevel != ToolAccessLevelReadOnly || r.maxTurns != 5 {
		t.Fatalf("registry config not refreshed: workdir=%q access=%q maxTurns=%d", r.workDir, r.toolAccessLevel, r.maxTurns)
	}
	if _, ok := r.agents["reviewer"]; !ok {
		t.Fatalf("agents not refreshed: %v", r.agents)
	}
	if r.sem == nil || cap(r.sem) != 3 {
		t.Fatalf("semaphore cap = %d, want 3", cap(r.sem))
	}
}

func TestSubAgentRegistryAsyncSpawnInheritsCurrentTurnReadOnlyClamp(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
		}},
	}
	runner := NewRunnerWithModel(model)
	readTool := &FunctionTool{
		ToolName:        "read",
		ToolDescription: "reads state",
		Schema:          json.RawMessage(`{"type":"object"}`),
		ReadOnly:        true,
		Fn:              func(context.Context, json.RawMessage) (string, error) { return "read", nil },
	}
	writeTool := &FunctionTool{
		ToolName:        "write",
		ToolDescription: "writes state",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn:              func(context.Context, json.RawMessage) (string, error) { return "wrote", nil },
	}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner:          runner,
		Agents:          map[string]*Agent{"analyst": {Name: "analyst", Tools: []Tool{readTool, writeTool}}},
		ToolAccessLevel: ToolAccessLevelFull,
	})
	ctx := WithNestedRunConfig(context.Background(), RunConfig{
		ToolAccessLevel:      ToolAccessLevelReadOnly,
		AllowedMutatingTools: []string{"write"}, // parent-only exception; children must not inherit it
	})

	taskID, err := registry.SpawnAsync(ctx, "analyst", "inspect", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminalTask(t, registry, taskID)

	if len(model.requests) != 1 || len(model.requests[0].Tools) != 1 || model.requests[0].Tools[0].Name() != "read" {
		t.Fatalf("async child model requests = %+v, want only the read tool", model.requests)
	}
}

func TestSubAgentRegistryCancelMarksTaskCancelledImmediately(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID:    "call-block",
					Name:  "block",
					Input: json.RawMessage(`{}`),
				}}},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
			},
		},
	}
	runner := NewRunnerWithModel(model)

	toolStarted := make(chan struct{})
	toolDone := make(chan struct{})
	var startedOnce sync.Once
	blockTool := &FunctionTool{
		ToolName:        "block",
		ToolDescription: "blocks until cancelled",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			startedOnce.Do(func() { close(toolStarted) })
			<-ctx.Done()
			close(toolDone)
			return "", ctx.Err()
		},
	}

	var events syncBuffer
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner:      runner,
		EventStream: NewEventStream(&events),
		Agents: map[string]*Agent{
			"analyst": {Name: "analyst", Tools: []Tool{blockTool}},
		},
	})

	taskID, err := registry.SpawnAsync(context.Background(), "analyst", "block until cancelled", "")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-toolStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocking tool to start")
	}

	if err := registry.Cancel(taskID); err != nil {
		t.Fatal(err)
	}

	task, err := registry.GetStatus(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCancelled {
		t.Fatalf("task status immediately after cancel = %s, want cancelled", task.Status)
	}

	select {
	case <-toolDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocking tool to observe cancellation")
	}

	// Cancel marks the task terminal immediately, but the single completion
	// event (with real usage) is written when the spawn goroutine unwinds.
	// Poll the (concurrency-safe) buffer for the event itself.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(events.String(), `"status":"cancelled"`) {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(events.String(), `"status":"cancelled"`) {
		t.Fatalf("event stream did not include cancelled status: %s", events.String())
	}
}

func TestSubAgentRegistrySetStatusDoesNotReopenCancelledTask(t *testing.T) {
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	registry.tasks["task_cancelled"] = &subAgentTaskEntry{
		task: SubAgentTask{
			ID:        "task_cancelled",
			Status:    SubAgentTaskCancelled,
			StartedAt: time.Now(),
			Error:     "cancellation requested",
		},
	}
	registry.order = append(registry.order, "task_cancelled")

	registry.setStatus("task_cancelled", SubAgentTaskRunning, "", "")

	task, err := registry.GetStatus("task_cancelled")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCancelled {
		t.Fatalf("task status after stale running update = %s, want cancelled", task.Status)
	}
}

func TestSubAgentRegistryPassesCompactionConfigToAsyncRuns(t *testing.T) {
	longOutput := strings.Repeat("tool output that should be compacted ", 20)
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID:    "call-1",
					Name:  "echo",
					Input: json.RawMessage(`{"n":1}`),
				}}},
			},
			{
				Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID:    "call-2",
					Name:  "echo",
					Input: json.RawMessage(`{"n":2}`),
				}}},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return longOutput, nil
		},
	}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: runner,
		Agents: map[string]*Agent{
			"analyst": {Name: "analyst", Tools: []Tool{echoTool}},
		},
		CompactionConfig: CompactionConfig{
			Enabled:                     true,
			TriggerTokens:               10,
			TargetTokens:                20,
			PreserveRecentItems:         1,
			PreserveInitialUserMessages: 1,
			SummaryBulletLimit:          1,
		},
	})

	taskID, err := registry.SpawnAsync(context.Background(), "analyst", "analyze this", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminalTask(t, registry, taskID)

	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(model.requests))
	}
	thirdInput := Items.ExtractText(model.requests[2].Input)
	if !strings.Contains(thirdInput, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("third request input = %q, want compacted history summary", thirdInput)
	}
}

func TestSubAgentRegistryPassesMaxTurnsToAsyncRuns(t *testing.T) {
	model := &mockModel{
		responses: make([]*ModelResponse, 10),
	}
	for i := range model.responses {
		model.responses[i] = &ModelResponse{
			Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "call",
				Name:  "echo",
				Input: json.RawMessage(`{"n":1}`),
			}}},
		}
	}
	runner := NewRunnerWithModel(model)
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "ok", nil
		},
	}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner:   runner,
		MaxTurns: 2,
		Agents: map[string]*Agent{
			"analyst": {Name: "analyst", Tools: []Tool{echoTool}},
		},
	})

	taskID, err := registry.SpawnAsync(context.Background(), "analyst", "loop", "")
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, err := registry.GetStatus(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.IsTerminal() {
			if task.Status != SubAgentTaskFailed {
				t.Fatalf("task status = %s, want failed", task.Status)
			}
			if !strings.Contains(task.Error, "max turns exceeded: 2") {
				t.Fatalf("task error = %q, want max turns exceeded: 2", task.Error)
			}
			if len(model.requests) != 2 {
				t.Fatalf("model requests = %d, want 2", len(model.requests))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := registry.GetStatus(taskID)
	t.Fatalf("timed out waiting for task %s; last status=%v", taskID, task)
}

func TestSubAgentRegistryWaitsForDependenciesAndInjectsResults(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "alpha result"}}},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "beta result"}}},
			},
		},
	}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{
			"analyst": {Name: "analyst"},
		},
	})

	firstID, err := registry.SpawnAsync(context.Background(), "analyst", "run alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := registry.SpawnAsyncWithOptions(context.Background(), "analyst", "run beta", SubAgentSpawnOptions{
		DependsOn: []string{firstID},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminalTask(t, registry, secondID)

	second, err := registry.GetStatus(secondID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.DependsOn) != 1 || second.DependsOn[0] != firstID {
		t.Fatalf("second.DependsOn = %v, want [%s]", second.DependsOn, firstID)
	}
	if len(model.requests) < 2 {
		t.Fatalf("model requests = %d, want at least 2", len(model.requests))
	}
	secondInput := Items.ExtractText(model.requests[1].Input)
	for _, want := range []string{"<sub_agent_dependency_results>", firstID, "alpha result"} {
		if !strings.Contains(secondInput, want) {
			t.Fatalf("second request input missing %q: %s", want, secondInput)
		}
	}
}

type liveSteeringModel struct {
	mu       sync.Mutex
	calls    int
	requests []ModelRequest
	started  chan struct{}
}

func (m *liveSteeringModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("GetResponse should not be called")
}

func (m *liveSteeringModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	if call == 1 {
		close(m.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	resp := &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "steered result"}}}}
	events := make(chan ModelStreamEvent, 2)
	done := make(chan *ModelResponse, 1)
	events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
	close(events)
	done <- resp
	return NewModelStream(events, done), nil
}

func (*liveSteeringModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (*liveSteeringModel) CalculateCost(Usage) float64            { return 0 }
func (*liveSteeringModel) Provider() string                       { return "test" }

func TestSubAgentRegistrySendMessageInterruptsInFlightModelAttempt(t *testing.T) {
	model := &liveSteeringModel{started: make(chan struct{})}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"analyst": {Name: "analyst"}},
	})

	taskID, err := registry.SpawnAsync(context.Background(), "analyst", "start", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first model attempt did not start")
	}
	if err := registry.SendMessage(taskID, "Conclude now with only blocking findings."); err != nil {
		t.Fatal(err)
	}
	waitForTerminalTask(t, registry, taskID)

	model.mu.Lock()
	requests := append([]ModelRequest(nil), model.requests...)
	model.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("model requests = %d, want interrupted attempt plus replacement", len(requests))
	}
	secondInput := Items.ExtractText(requests[1].Input)
	for _, want := range []string{"[PARENT MESSAGE]", "Conclude now with only blocking findings."} {
		if !strings.Contains(secondInput, want) {
			t.Fatalf("replacement request input missing %q: %s", want, secondInput)
		}
	}
}

func TestSubAgentRegistrySendMessageQueuesParentMessage(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID:    "call-block",
					Name:  "block",
					Input: json.RawMessage(`{}`),
				}}},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	var startedOnce sync.Once
	blockTool := &FunctionTool{
		ToolName:        "block",
		ToolDescription: "blocks until released",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			startedOnce.Do(func() { close(toolStarted) })
			select {
			case <-releaseTool:
				return "released", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: runner,
		Agents: map[string]*Agent{
			"analyst": {Name: "analyst", Tools: []Tool{blockTool}},
		},
	})

	taskID, err := registry.SpawnAsync(context.Background(), "analyst", "start", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-toolStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if err := registry.SendMessage(taskID, "Use the narrower API boundary."); err != nil {
		t.Fatal(err)
	}
	close(releaseTool)
	waitForTerminalTask(t, registry, taskID)

	task, err := registry.GetStatus(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.MessagesReceived != 1 || !strings.Contains(task.LastParentMessage, "narrower API") {
		t.Fatalf("message fields = received %d last %q", task.MessagesReceived, task.LastParentMessage)
	}
	if len(model.requests) < 2 {
		t.Fatalf("model requests = %d, want at least 2", len(model.requests))
	}
	secondInput := Items.ExtractText(model.requests[1].Input)
	for _, want := range []string{"[PARENT MESSAGE]", "Use the narrower API boundary."} {
		if !strings.Contains(secondInput, want) {
			t.Fatalf("second request input missing %q: %s", want, secondInput)
		}
	}
}

func waitForTerminalTask(t *testing.T, registry *SubAgentRegistry, taskID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, err := registry.GetStatus(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.IsTerminal() {
			if task.Status != SubAgentTaskCompleted {
				t.Fatalf("task status = %s, error=%s", task.Status, task.Error)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := registry.GetStatus(taskID)
	t.Fatalf("timed out waiting for task %s; last status=%v", taskID, task)
}

func TestContainsAny(t *testing.T) {
	if !containsAny("git commit -m fix", "git commit") {
		t.Error("should match git commit")
	}
	if !containsAny("git add .", "git add") {
		t.Error("should match git add")
	}
	if containsAny("go build", "git commit", "git add") {
		t.Error("should not match go build")
	}
	if containsAny("", "git commit") {
		t.Error("should not match empty string")
	}
}

func TestSubAgentActivity_RecordToolEnd_TracksReads(t *testing.T) {
	a := NewSubAgentActivity()

	a.RecordToolEnd("read_file", "internal/agent/runner.go", false, 10)
	a.RecordToolEnd("read_file", "internal/agent/runner.go", false, 10) // duplicate
	a.RecordToolEnd("read_file", "cmd/agent/plan.go", false, 10)
	a.RecordToolEnd("read_file", "broken.go", true, 10) // error — not tracked
	a.RecordToolEnd("read_file", "", false, 10)         // no path — not tracked

	snap := a.Snapshot(false)
	if len(snap.FilesRead) != 2 {
		t.Fatalf("expected 2 files read, got %d: %v", len(snap.FilesRead), snap.FilesRead)
	}
	if snap.FilesRead[0] != "internal/agent/runner.go" || snap.FilesRead[1] != "cmd/agent/plan.go" {
		t.Errorf("unexpected files read: %v", snap.FilesRead)
	}
}

func TestSubAgentRegistrySpawnUsesShortTaskIDs(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
	}}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"analyst": {Name: "analyst"}},
	})
	taskID, err := registry.SpawnAsync(context.Background(), "analyst", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(taskID, "task_") {
		t.Fatalf("task ID %q missing task_ prefix", taskID)
	}
	if len(taskID) != len("task_")+8 {
		t.Fatalf("task ID %q should be short (task_ + 8 chars), got len %d", taskID, len(taskID))
	}
}

func TestSubAgentRegistryConcurrentConfigureAndSpawn(t *testing.T) {
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Agents: map[string]*Agent{"a": {Name: "a"}},
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			registry.Configure(SubAgentRegistryConfig{
				Agents:        map[string]*Agent{"a": {Name: "a"}},
				MaxConcurrent: 2,
			})
			registry.SetAllowedAgents([]string{"a"})
		}
	}()
	for i := 0; i < 200; i++ {
		// Unknown agent: exercises the locked r.agents read without launching goroutines.
		_, _ = registry.SpawnAsyncWithOptions(context.Background(), "missing", "msg", SubAgentSpawnOptions{})
	}
	<-done
}

func TestSubAgentRegistrySchedulerCheckpointRestore(t *testing.T) {
	startedAt := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	source := NewSubAgentRegistry(SubAgentRegistryConfig{})
	source.tasks = map[string]*subAgentTaskEntry{
		"task_completed": {
			task: SubAgentTask{
				ID: "task_completed", AgentName: "writer", Status: SubAgentTaskCompleted,
				Message: "write", StartedAt: startedAt, Duration: time.Second,
				Result: "finished", ToolCount: 3, Tokens: 42, DependsOn: []string{"task_failed"},
				DependencyPolicy: SubAgentDependencyAllTerminal, MessagesReceived: 2,
				LastParentMessage: "continue",
			},
			includeDependencyResults: true,
			resultDelivered:          true,
		},
		"task_failed": {
			task: SubAgentTask{
				ID: "task_failed", AgentName: "reviewer", Status: SubAgentTaskFailed,
				Message: "review", StartedAt: startedAt.Add(time.Second), Duration: 2 * time.Second,
				Error: "review failed", ToolCount: 1, Tokens: 8,
			},
			resultDelivered: false,
		},
		"task_cancelled": {
			task: SubAgentTask{
				ID: "task_cancelled", AgentName: "planner", Status: SubAgentTaskCancelled,
				Message: "plan", StartedAt: startedAt.Add(2 * time.Second), Duration: 3 * time.Second,
				Result: "partial plan", Error: "cancelled", ToolCount: 2, Tokens: 12,
			},
			resultDelivered: false,
		},
		"task_pending": {
			task: SubAgentTask{
				ID: "task_pending", AgentName: "worker", Status: SubAgentTaskPending,
				Message: "queue", StartedAt: startedAt.Add(3 * time.Second),
			},
		},
		"task_waiting": {
			task: SubAgentTask{
				ID: "task_waiting", AgentName: "worker", Status: SubAgentTaskWaiting,
				Message: "wait", StartedAt: startedAt.Add(4 * time.Second), WaitingOn: []string{"task_pending"},
			},
			includeDependencyResults: true,
		},
		"task_running": {
			task: SubAgentTask{
				ID: "task_running", AgentName: "worker", Status: SubAgentTaskRunning,
				Message: "work", StartedAt: startedAt.Add(5 * time.Second), WaitingOn: []string{"task_waiting"},
			},
		},
	}
	source.order = []string{"task_completed", "task_failed", "task_cancelled", "task_pending", "task_waiting", "task_running"}

	encoded, err := json.Marshal(source.SchedulerCheckpoint())
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint SubAgentSchedulerCheckpoint
	if err := json.Unmarshal(encoded, &checkpoint); err != nil {
		t.Fatal(err)
	}

	restored := NewSubAgentRegistry(SubAgentRegistryConfig{})
	if err := restored.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}

	tasks := restored.ListTasks()
	if len(tasks) != 6 {
		t.Fatalf("restored task count = %d, want 6", len(tasks))
	}
	for i, want := range []string{"task_completed", "task_failed", "task_cancelled", "task_pending", "task_waiting", "task_running"} {
		if tasks[i].ID != want {
			t.Fatalf("restored order = %v, want task %q at index %d", tasks, want, i)
		}
	}
	if got := tasks[0]; got.Status != SubAgentTaskCompleted || got.Result != "finished" || got.Error != "" || got.AgentName != "writer" || got.MessagesReceived != 2 || got.LastParentMessage != "continue" {
		t.Fatalf("completed task not preserved: %+v", got)
	}
	if got := tasks[1]; got.Status != SubAgentTaskFailed || got.Error != "review failed" || got.ToolCount != 1 || got.Tokens != 8 {
		t.Fatalf("failed task not preserved: %+v", got)
	}
	if got := tasks[2]; got.Status != SubAgentTaskCancelled || got.Result != "partial plan" || got.Error != "cancelled" || got.ToolCount != 2 || got.Tokens != 12 {
		t.Fatalf("cancelled task not preserved: %+v", got)
	}
	for _, task := range tasks[3:] {
		if task.Status != SubAgentTaskFailed || task.Error != subAgentRuntimeRestartError || len(task.WaitingOn) != 0 {
			t.Fatalf("active task was not restored as restart tombstone: %+v", task)
		}
	}
	if pending := restored.PendingResultTaskIDs(); strings.Join(pending, ",") != "task_failed,task_cancelled,task_pending,task_waiting,task_running" {
		t.Fatalf("pending result task IDs = %v, want restored undelivered terminal tasks", pending)
	}
	if task, err := restored.WaitForTask(context.Background(), "task_running", 1); err != nil || task.Status != SubAgentTaskFailed {
		t.Fatalf("wait on restart tombstone = %+v, %v", task, err)
	}
	if task, firstDelivery, err := restored.CollectResultIfUndelivered("task_completed"); err != nil || firstDelivery || task.Result != "finished" {
		t.Fatalf("completed delivery state not preserved: task=%+v first=%v err=%v", task, firstDelivery, err)
	}
	if task, firstDelivery, err := restored.CollectResultIfUndelivered("task_failed"); err != nil || !firstDelivery || task.Error != "review failed" {
		t.Fatalf("failed delivery state not preserved: task=%+v first=%v err=%v", task, firstDelivery, err)
	}
}

func TestSubAgentRegistryRestoreSchedulerCheckpointRejectsNonFreshAndDuplicateIDs(t *testing.T) {
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	registry.tasks["existing"] = &subAgentTaskEntry{task: SubAgentTask{ID: "existing", Status: SubAgentTaskCompleted}}
	registry.order = []string{"existing"}
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task: SubAgentTask{ID: "task_1", Status: SubAgentTaskCompleted},
	}}}
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err == nil || !strings.Contains(err.Error(), "empty registry") {
		t.Fatalf("restore into non-fresh registry error = %v", err)
	}
	if task, err := registry.GetStatus("existing"); err != nil || task.Status != SubAgentTaskCompleted {
		t.Fatalf("non-fresh registry was changed: task=%+v err=%v", task, err)
	}

	fresh := NewSubAgentRegistry(SubAgentRegistryConfig{})
	duplicate := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{
		{Task: SubAgentTask{ID: "task_1", Status: SubAgentTaskCompleted}},
		{Task: SubAgentTask{ID: "task_1", Status: SubAgentTaskFailed}},
	}}
	if err := fresh.RestoreSchedulerCheckpoint(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate checkpoint error = %v", err)
	}
	if len(fresh.ListTasks()) != 0 {
		t.Fatalf("duplicate checkpoint partially restored tasks: %+v", fresh.ListTasks())
	}
}

func TestSubAgentRegistryBroadcastWakesAllWaiters(t *testing.T) {
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	registry.tasks["task_wait"] = &subAgentTaskEntry{
		task: SubAgentTask{ID: "task_wait", Status: SubAgentTaskRunning, StartedAt: time.Now()},
	}
	registry.order = append(registry.order, "task_wait")

	const waiters = 4
	results := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			task, err := registry.WaitForTask(context.Background(), "task_wait", 5000)
			if err != nil {
				results <- err
				return
			}
			if !task.IsTerminal() {
				results <- context.DeadlineExceeded
				return
			}
			results <- nil
		}()
	}

	time.Sleep(50 * time.Millisecond)
	registry.setTerminal("task_wait", SubAgentTaskCompleted, "ok", "", time.Second, 0, 0)

	// All waiters must wake from the single state change well before the 5s
	// wait deadline — the old buffered-1 channel woke only one of them.
	deadline := time.After(2 * time.Second)
	for i := 0; i < waiters; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("waiter %d failed: %v", i, err)
			}
		case <-deadline:
			t.Fatalf("waiter %d not woken by broadcast", i)
		}
	}
}
