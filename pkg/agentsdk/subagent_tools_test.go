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

func TestRunSubagentTaskToolWaitsAndReturnsResult(t *testing.T) {
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
	tool := &runSubagentTaskTool{registry: registry, defaultAgent: "worker"}

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

func TestSpawnSubagentTaskManagedJoinProviderReturnsResultOnce(t *testing.T) {
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
	tool := &spawnSubagentTaskTool{registry: registry, defaultAgent: "worker"}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"do it"}`), "")
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

func TestSpawnSubagentTaskManagedJoinProviderWaitsInSDKUntilResult(t *testing.T) {
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
	tool := &spawnSubagentTaskTool{registry: registry, defaultAgent: "worker"}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"do it"}`), "")
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

func TestWaitForSubagentProgressReturnsReportableSnapshot(t *testing.T) {
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

	tool := &waitForSubagentProgressTool{registry: registry}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"timeout_ms":1}`), "")
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

func TestGetSubagentTaskStatusTruncatesMessageEcho(t *testing.T) {
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

	tool := &getSubagentTaskStatusTool{registry: registry}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"task_id":"`+taskID+`"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var view struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(result.Content), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Message) > 260 {
		t.Fatalf("status echo should truncate message to ~240 chars, got %d", len(view.Message))
	}
	if !strings.HasSuffix(view.Message, "...") {
		t.Fatalf("truncated message should end with ellipsis: %q", view.Message)
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
