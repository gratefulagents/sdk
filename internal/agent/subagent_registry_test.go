package agent

import (
	"bytes"
	"context"
	"encoding/json"
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
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("expected budget context to contain %q, got %s", want, ctx)
		}
	}
}

func TestBuildRunBudgetContext(t *testing.T) {
	ctx := BuildRunBudgetContext(23)
	for _, want := range []string{
		"Turn budget: 23 LLM turns for this top-level run",
		"not one tool call",
		"hard ceiling, not a target",
		"complete and verified",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("expected run budget context to contain %q, got %s", want, ctx)
		}
	}
	if strings.Contains(ctx, "sub-agent") || strings.Contains(ctx, "<sub_agent_budget>") {
		t.Fatalf("run budget must not use sub-agent language: %s", ctx)
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

	// Wait for the spawn goroutine to finish writing the cancellation event
	// before reading the buffer (bytes.Buffer is not safe for concurrent use).
	deadline := time.Now().Add(2 * time.Second)
	for {
		t, _ := registry.GetStatus(taskID)
		if t.IsTerminal() {
			break
		}
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
