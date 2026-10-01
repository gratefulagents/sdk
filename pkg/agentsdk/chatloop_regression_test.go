package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/durable"
)

type recordingSessionStore struct {
	mu    sync.Mutex
	items []RunItem
	// ctxErrs records the context state seen by each append.
	ctxErrs []error
}

func (s *recordingSessionStore) LoadMessages(_ context.Context, cursor Cursor, _ int) ([]UserMessage, Cursor, error) {
	if cursor.MessageID > 0 {
		return nil, cursor, nil
	}
	return []UserMessage{{ID: 1, Content: "do the work"}}, Cursor{MessageID: 1}, nil
}

func (s *recordingSessionStore) AppendRunItems(ctx context.Context, items []RunItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	s.items = append(s.items, items...)
	return nil
}

func (s *recordingSessionStore) WorkingState(context.Context) (WorkingState, error) {
	return WorkingState{}, nil
}

// assertToolCallsPaired fails when any persisted tool call lacks an output.
func assertToolCallsPaired(t *testing.T, items []RunItem) {
	t.Helper()
	outputs := map[string]bool{}
	for _, item := range items {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil {
			outputs[item.ToolOutput.CallID] = true
		}
	}
	for _, item := range items {
		if item.Type == RunItemToolCall && item.ToolCall != nil && !outputs[item.ToolCall.ID] {
			t.Fatalf("tool call %s persisted without an output; items=%+v", item.ToolCall.ID, items)
		}
	}
}

func toolCallResponse(id, name string) *ModelResponse {
	return &ModelResponse{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: id, Name: name, Input: json.RawMessage(`{}`)}}}}
}

func messageResponse(text string) *ModelResponse {
	return &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: text}}}}
}

func okTool(name string, fn func()) *FunctionTool {
	return &FunctionTool{ToolName: name, Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) {
		if fn != nil {
			fn()
		}
		return name + " ok", nil
	}}
}

func TestChatLoopPersistsPartialResultWhenRunFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &scriptedChatModel{responses: []*ModelResponse{
		toolCallResponse("write_1", "write"),
		messageResponse("never reached"),
	}}
	store := &recordingSessionStore{}
	// The tool's side effect happens, then the host shuts down mid-turn.
	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{okTool("write", cancel)}},
		RunConfig:    RunConfig{MaxTurns: 3},
		SessionStore: store,
	}).Run(ctx)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if result == nil {
		t.Fatal("partial result dropped")
	}
	if !containsToolOutput(result.NewItems, "write ok") {
		t.Fatalf("partial result lacks completed tool output: %+v", result.NewItems)
	}
	if !containsToolOutput(store.items, "write ok") {
		t.Fatalf("completed tool output not persisted: %+v", store.items)
	}
	for _, ctxErr := range store.ctxErrs {
		if ctxErr != nil {
			t.Fatalf("partial items persisted on a cancelled context: %v", ctxErr)
		}
	}
}

type failingGate struct{ calls int }

func (g *failingGate) ApproveTool(context.Context, ToolApprovalRequest) (bool, string, error) {
	g.calls++
	return false, "", errors.New("approval service unavailable")
}

func TestChatLoopPairsRemainingCallsWhenApprovalResolutionFails(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{{Items: []RunItem{
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "mutate_a", Input: json.RawMessage(`{}`)}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "mutate_b", Input: json.RawMessage(`{}`)}},
	}}}}
	store := &recordingSessionStore{}
	gate := &failingGate{}
	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{okTool("mutate_a", nil), okTool("mutate_b", nil)}},
		RunConfig:    RunConfig{MaxTurns: 3, ToolPolicy: &ToolPolicy{ApprovalRequired: true}},
		SessionStore: store,
		ApprovalGate: gate,
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "approval service unavailable") {
		t.Fatalf("err = %v", err)
	}
	if gate.calls != 1 {
		t.Fatalf("gate calls = %d, want 1", gate.calls)
	}
	assertToolCallsPaired(t, store.items)
	assertToolCallsPaired(t, result.NewItems)
	assertToolCallsPaired(t, result.FinalHistory)
	if result.IsInterrupted() {
		t.Fatal("settled approvals still reported as interrupted")
	}
}

func TestChatLoopApprovalRoundsWithProgressDoNotHitResumeLimit(t *testing.T) {
	const edits = 5
	var responses []*ModelResponse
	for i := 0; i < edits; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("edit_%d", i), "edit"))
	}
	responses = append(responses, messageResponse("all edited"))
	model := &scriptedChatModel{responses: responses}
	executed := 0
	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{okTool("edit", func() { executed++ })}},
		RunConfig:    RunConfig{MaxTurns: 20, ToolPolicy: &ToolPolicy{ApprovalRequired: true}},
		ApprovalGate: approvingGate{approved: true},
		MaxResumes:   2,
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if executed != edits || result.FinalText() != "all edited" {
		t.Fatalf("executed=%d final=%q", executed, result.FinalText())
	}
}

func TestChatLoopResumeLimitPairsPendingCalls(t *testing.T) {
	var responses []*ModelResponse
	for i := 0; i < 6; i++ {
		responses = append(responses, toolCallResponse(fmt.Sprintf("edit_%d", i), "edit"))
	}
	model := &scriptedChatModel{responses: responses}
	store := &recordingSessionStore{}
	_, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{okTool("edit", nil)}},
		RunConfig:    RunConfig{MaxTurns: 20, ToolPolicy: &ToolPolicy{ApprovalRequired: true}},
		SessionStore: store,
		ApprovalGate: approvingGate{approved: false},
		MaxResumes:   2,
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "too many chat loop resumes") {
		t.Fatalf("err = %v", err)
	}
	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want 3 (limit after 2 stalled rounds)", len(model.requests))
	}
	assertToolCallsPaired(t, store.items)
}

func TestChatLoopDurableDeniedApprovalContinuesWithoutDoubleCountingUsage(t *testing.T) {
	ctx := context.Background()
	fs, err := durable.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenStoredRun(ctx, fs, StoredRunOptions{TenantID: "tenant_a", RunID: "run_a", Owner: "worker", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close(ctx)

	call := toolCallResponse("rm_call", "rm")
	call.Usage = Usage{Requests: 1, InputTokens: 100, OutputTokens: 10}
	final := messageResponse("understood, not deleting")
	final.Usage = Usage{Requests: 1, InputTokens: 50, OutputTokens: 5}
	model := &scriptedChatModel{responses: []*ModelResponse{call, final}}
	executed := false
	durableCfg := run.RunConfig()
	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{okTool("rm", func() { executed = true })}},
		RunConfig:    RunConfig{MaxTurns: 5, ToolPolicy: &ToolPolicy{ApprovalRequired: true}, Durable: durableCfg},
		ApprovalGate: approvingGate{approved: false},
	}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatal("denied tool executed")
	}
	if result.FinalText() != "understood, not deleting" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
	if result.Usage.InputTokens != 150 || result.Usage.OutputTokens != 15 || result.Usage.Requests != 2 {
		t.Fatalf("usage = %+v, want 150/15 over 2 requests", result.Usage)
	}
	// The resumed model call must see the denial paired with the tool call.
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d", len(model.requests))
	}
	var resumedInput []RunItem
	if durableCfg.Resume != nil {
		resumedInput, _ = RestoreRunItems(durableCfg.Resume.History, nil)
	}
	assertToolCallsPaired(t, resumedInput)
	snapshot, _, err := fs.Load(ctx, "tenant_a", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != durable.RunSucceeded || snapshot.CumulativeBudget.InputTokens != 150 || snapshot.CumulativeBudget.OutputTokens != 15 {
		t.Fatalf("snapshot status=%s budget=%+v", snapshot.Status, snapshot.CumulativeBudget)
	}
}

func TestChatLoopCancelsBackgroundSubAgentsWhenCancelled(t *testing.T) {
	child := &blockingSubagentToolModel{started: make(chan struct{}), release: make(chan struct{})}
	defer close(child.release)
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(child),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	taskID, err := registry.SpawnAsyncWithOptions(context.Background(), "worker", "long job", SubAgentSpawnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child never started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = NewChatLoop(ChatLoopOptions{
		Runner: NewRunnerWithModel(&scriptedChatModel{}),
		Agent:  &Agent{Name: "loop", Model: "demo", Tools: BuildSubAgentTaskTools(registry, "worker")},
	}).Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := registry.GetStatus(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == SubAgentTaskCancelled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background sub-agent still %s after ChatLoop cancellation", task.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChatLoopPreservesExecutedOutputWhenCompletionCheckpointFails(t *testing.T) {
	writes := 0
	failed := false
	var saved DurableCheckpoint
	cfg := &DurableRunConfig{
		RunID: "approved-effect", AttemptID: "attempt",
		Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
			if cp.Boundary == DurableBoundaryToolCompleted && writes == 1 && !failed {
				failed = true
				return errors.New("temporary storage failure after execution")
			}
			saved = cp
			return nil
		},
	}
	model := &scriptedChatModel{responses: []*ModelResponse{toolCallResponse("write-1", "write")}}
	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{okTool("write", func() { writes++ })}},
		RunConfig:    RunConfig{MaxTurns: 3, ToolPolicy: &ToolPolicy{ApprovalRequired: true}, Durable: cfg},
		ApprovalGate: approvingGate{approved: true},
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "persist approved tool completion") {
		t.Fatalf("error = %v", err)
	}
	if writes != 1 || saved.Boundary != DurableBoundaryToolCompleted {
		t.Fatalf("writes=%d boundary=%s", writes, saved.Boundary)
	}
	assertToolCallsPaired(t, result.FinalHistory)
	found := false
	for _, item := range saved.History {
		if item.ToolOutput != nil && item.ToolOutput.CallID == "write-1" {
			found = true
			if item.ToolOutput.IsError || item.ToolOutput.Content != "write ok" {
				t.Fatalf("executed tool misrepresented: %+v", item.ToolOutput)
			}
		}
	}
	if !found {
		t.Fatal("executed output absent from checkpoint")
	}
}
