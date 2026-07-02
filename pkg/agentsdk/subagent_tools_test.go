package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type subagentToolMockModel struct {
	mu        sync.Mutex
	responses []*ModelResponse
	callIdx   int
}

func (m *subagentToolMockModel) GetResponse(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.callIdx
	m.callIdx++
	if idx >= len(m.responses) {
		return nil, errors.New("no more responses")
	}
	return m.responses[idx], nil
}

func (m *subagentToolMockModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	resp, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan ModelStreamEvent, 1)
	done := make(chan *ModelResponse, 1)
	go func() {
		defer close(events)
		events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
		done <- resp
	}()
	return NewModelStream(events, done), nil
}

func (m *subagentToolMockModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (m *subagentToolMockModel) CalculateCost(Usage) float64            { return 0 }
func (m *subagentToolMockModel) Provider() string                       { return "mock" }

type blockingSubagentToolModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingSubagentToolModel) GetResponse(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	m.once.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "blocked child done"}}}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *blockingSubagentToolModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	resp, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan ModelStreamEvent, 1)
	done := make(chan *ModelResponse, 1)
	go func() {
		defer close(events)
		events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
		done <- resp
	}()
	return NewModelStream(events, done), nil
}

func (m *blockingSubagentToolModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (m *blockingSubagentToolModel) CalculateCost(Usage) float64            { return 0 }
func (m *blockingSubagentToolModel) Provider() string                       { return "mock" }

func TestSubagentToolSyncModeWaitsAndReturnsResult(t *testing.T) {
	model := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "child done"}}}},
		},
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{
			"worker": {Name: "worker"},
		},
	})
	tool := &subagentTool{registry: registry, defaultAgent: "worker"}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"do it"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %s", result.Content)
	}
	if !strings.Contains(result.Content, `"status": "completed"`) || !strings.Contains(result.Content, `"result": "child done"`) {
		t.Fatalf("content = %s", result.Content)
	}
}

func TestSubagentToolRequiresExactlyOneOfMessageOrTasks(t *testing.T) {
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(&subagentToolMockModel{}),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	tool := &subagentTool{registry: registry, defaultAgent: "worker"}

	for name, input := range map[string]string{
		"neither": `{}`,
		"both":    `{"message":"x","tasks":[{"key":"a","message":"y"}]}`,
	} {
		result, err := tool.Execute(context.Background(), json.RawMessage(input), "")
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError || !strings.Contains(result.Content, "exactly one of") {
			t.Fatalf("%s: result = %+v, want mutual-exclusion error", name, result)
		}
	}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"x","mode":"nope"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "invalid mode") {
		t.Fatalf("invalid mode result = %+v", result)
	}
}

func TestSubagentToolBackgroundModeManagedJoinProviderReturnsResultOnce(t *testing.T) {
	model := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "managed child"}}}},
		},
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{
			"worker": {Name: "worker"},
		},
	})
	tool := &subagentTool{registry: registry, defaultAgent: "worker"}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"do it","mode":"background"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("spawn failed: %s", result.Content)
	}
	var spawned struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(result.Content), &spawned); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.WaitForTask(context.Background(), spawned.TaskID, 1000); err != nil {
		t.Fatal(err)
	}
	items, err := tool.JoinSubAgentResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Message == nil || !strings.Contains(items[0].Message.Text, "managed child") {
		t.Fatalf("joined items = %#v", items)
	}
	items, err = tool.JoinSubAgentResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("second join returned %#v, want no duplicate results", items)
	}
}

func TestSubagentToolBackgroundModeManagedJoinProviderWaitsInSDKUntilResult(t *testing.T) {
	model := &blockingSubagentToolModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{
			"worker": {Name: "worker"},
		},
	})
	tool := &subagentTool{registry: registry, defaultAgent: "worker"}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"do it","mode":"background"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("spawn failed: %s", result.Content)
	}
	var spawned struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(result.Content), &spawned); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for child to start")
	}

	type joinResult struct {
		items []RunItem
		err   error
	}
	done := make(chan joinResult, 1)
	go func() {
		items, err := tool.JoinSubAgentResults(context.Background())
		done <- joinResult{items: items, err: err}
	}()

	select {
	case got := <-done:
		t.Fatalf("JoinSubAgentResults returned while task was active: items=%#v err=%v", got.items, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	close(model.release)
	if _, err := registry.WaitForTask(context.Background(), spawned.TaskID, 1000); err != nil {
		t.Fatal(err)
	}
	var got joinResult
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for JoinSubAgentResults after child completion")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	items := got.items
	if len(items) != 1 || items[0].Message == nil || !strings.Contains(items[0].Message.Text, "blocked child done") {
		t.Fatalf("joined items after completion = %#v", items)
	}
}

func TestSubagentToolBatchSyncWaitsForWholeGraph(t *testing.T) {
	model := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "first result"}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "second result"}}}},
		},
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	tool := &subagentTool{registry: registry, defaultAgent: "worker"}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"tasks": [
			{"key": "a", "message": "first"},
			{"key": "b", "message": "second", "depends_on": ["a"]}
		]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("batch sync failed: %s", result.Content)
	}
	for _, want := range []string{`"wait_complete": true`, `"task_ids_by_key"`, `"results"`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("content missing %s: %s", want, result.Content)
		}
	}
	if strings.Count(result.Content, `"status": "completed"`) != 2 {
		t.Fatalf("expected two completed results: %s", result.Content)
	}
	// Sync batch results are marked delivered — no duplicate final-join delivery.
	if tool.HasPendingSubAgentFinalJoin() {
		t.Fatal("no pending final join expected after sync batch")
	}
}

func TestSubagentToolBatchRejectsCyclesAndUnknownDeps(t *testing.T) {
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(&subagentToolMockModel{}),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	tool := &subagentTool{registry: registry, defaultAgent: "worker"}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"tasks": [
			{"key": "a", "message": "x", "depends_on": ["b"]},
			{"key": "b", "message": "y", "depends_on": ["a"]}
		]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "cycle") {
		t.Fatalf("cycle result = %+v", result)
	}

	result, err = tool.Execute(context.Background(), json.RawMessage(`{
		"tasks": [{"key": "a", "message": "x", "depends_on": ["ghost"]}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "neither a key in this batch nor an existing task id") {
		t.Fatalf("unknown dep result = %+v", result)
	}
}

func TestSubagentStatusSummaryReturnsReportableSnapshot(t *testing.T) {
	model := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "finished child"}}}},
		},
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{
			"worker": {Name: "worker"},
		},
	})
	taskID, err := registry.SpawnAsyncWithOptions(context.Background(), "worker", "do it", SubAgentSpawnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.WaitForTask(context.Background(), taskID, 1000); err != nil {
		t.Fatal(err)
	}

	tool := &subagentStatusTool{registry: registry}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %s", result.Content)
	}
	for _, want := range []string{`"summary"`, `"tasks"`, `"completed": 1`, `"result_available": true`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("content missing %s: %s", want, result.Content)
		}
	}
	if strings.Contains(result.Content, "finished child") {
		t.Fatalf("progress snapshot leaked final result content: %s", result.Content)
	}
}

func TestSubagentStatusDetailsActivityAndGraph(t *testing.T) {
	model := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})

	longMessage := strings.Repeat("task packet detail ", 100) // ~1900 chars
	taskID, err := registry.SpawnAsync(context.Background(), "worker", longMessage, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.WaitForTask(context.Background(), taskID, 5000); err != nil {
		t.Fatal(err)
	}

	tool := &subagentStatusTool{registry: registry}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"activity","task_ids":["`+taskID+`"]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("activity error: %s", result.Content)
	}
	if !strings.Contains(result.Content, `"activity"`) || !strings.Contains(result.Content, taskID) {
		t.Fatalf("activity content = %s", result.Content)
	}
	// Status views must not echo the full task packet back into the parent context.
	if strings.Contains(result.Content, longMessage) {
		t.Fatal("activity view leaked full task message")
	}

	result, err = tool.Execute(context.Background(), json.RawMessage(`{"detail":"graph"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("graph error: %s", result.Content)
	}
	if !strings.Contains(result.Content, `"nodes"`) || !strings.Contains(result.Content, `"edges"`) {
		t.Fatalf("graph content = %s", result.Content)
	}

	result, err = tool.Execute(context.Background(), json.RawMessage(`{"detail":"bogus"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "invalid detail") {
		t.Fatalf("invalid detail result = %+v", result)
	}
}

func TestSubagentControlMessageAndCancel(t *testing.T) {
	model := &blockingSubagentToolModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	taskID, err := registry.SpawnAsync(context.Background(), "worker", "long task", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for child to start")
	}

	tool := &subagentControlTool{registry: registry}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"message","task_id":"`+taskID+`","message":"narrow the scope"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, "message_queued") {
		t.Fatalf("message result = %+v", result)
	}

	result, err = tool.Execute(context.Background(), json.RawMessage(`{"action":"message","task_id":"`+taskID+`"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "message is required") {
		t.Fatalf("empty message result = %+v", result)
	}

	result, err = tool.Execute(context.Background(), json.RawMessage(`{"action":"bogus","task_id":"`+taskID+`"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "invalid action") {
		t.Fatalf("invalid action result = %+v", result)
	}

	result, err = tool.Execute(context.Background(), json.RawMessage(`{"action":"cancel","task_id":"`+taskID+`"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, "cancellation_requested") {
		t.Fatalf("cancel result = %+v", result)
	}
	task, err := registry.WaitForTask(context.Background(), taskID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCancelled {
		t.Fatalf("task.Status = %s, want cancelled", task.Status)
	}
}

func TestSubagentResultsDeliveredIncrementallyAtTurnBoundary(t *testing.T) {
	childModel := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "early child result"}}}},
		},
	}
	childRunner := NewRunnerWithModel(childModel)
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: childRunner,
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})

	taskID, err := registry.SpawnAsync(context.Background(), "worker", "fast task", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.WaitForTask(context.Background(), taskID, 5000); err != nil {
		t.Fatal(err)
	}

	// Parent model: a tool-free turn followed by a final answer. The runner
	// must inject the completed task result before the next model call.
	parentModel := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "parent done"}}}},
		},
	}
	parentRunner := NewRunnerWithModel(parentModel)
	parent := &Agent{Name: "parent", Tools: BuildSubAgentTaskTools(registry, "worker")}

	result, err := parentRunner.Run(context.Background(), parent, []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "supervise"}},
	}, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}

	injected := false
	for _, item := range result.NewItems {
		if item.Type == RunItemMessage && item.Message != nil &&
			strings.Contains(item.Message.Text, "<sub_agent_results>") &&
			strings.Contains(item.Message.Text, "early child result") {
			injected = true
		}
	}
	if !injected {
		t.Fatal("terminal sub-agent result was not injected incrementally at the turn boundary")
	}
}
