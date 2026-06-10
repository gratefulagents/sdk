package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestExecuteToolsSerializesMutatingTools verifies that non-read-only tools
// never overlap in time, while read-only tools run concurrently.
func TestExecuteToolsSerializesMutatingTools(t *testing.T) {
	var active, maxActive int32
	mutating := func(name string) *FunctionTool {
		return &FunctionTool{
			ToolName:        name,
			ToolDescription: "mutating",
			Schema:          json.RawMessage(`{"type":"object"}`),
			Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
				cur := atomic.AddInt32(&active, 1)
				for {
					prev := atomic.LoadInt32(&maxActive)
					if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
						break
					}
				}
				time.Sleep(30 * time.Millisecond)
				atomic.AddInt32(&active, -1)
				return "ok", nil
			},
		}
	}
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "mut_a", Input: json.RawMessage(`{}`)}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "mut_b", Input: json.RawMessage(`{}`)}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c3", Name: "mut_c", Input: json.RawMessage(`{}`)}},
			}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{mutating("mut_a"), mutating("mut_b"), mutating("mut_c")}}

	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("mutating tools overlapped: max concurrent = %d, want 1", got)
	}
}

func TestExecuteToolsRunsReadOnlyToolsInParallel(t *testing.T) {
	var active, maxActive int32
	var mu sync.Mutex
	barrier := make(chan struct{})
	started := 0
	readOnly := func(name string) *FunctionTool {
		return &FunctionTool{
			ToolName:        name,
			ToolDescription: "read-only",
			Schema:          json.RawMessage(`{"type":"object"}`),
			ReadOnly:        true,
			Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
				cur := atomic.AddInt32(&active, 1)
				for {
					prev := atomic.LoadInt32(&maxActive)
					if cur <= prev || atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
						break
					}
				}
				mu.Lock()
				started++
				if started == 2 {
					close(barrier)
				}
				mu.Unlock()
				// Wait for the sibling: only succeeds if both run concurrently.
				select {
				case <-barrier:
				case <-time.After(2 * time.Second):
				}
				atomic.AddInt32(&active, -1)
				return "ok", nil
			},
		}
	}
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "ro_a", Input: json.RawMessage(`{}`)}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "ro_b", Input: json.RawMessage(`{}`)}},
			}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{readOnly("ro_a"), readOnly("ro_b")}}

	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&maxActive); got != 2 {
		t.Fatalf("read-only tools should run concurrently: max concurrent = %d, want 2", got)
	}
}

// TestRunnerCapsModelFacingToolOutput verifies oversized tool output is
// middle-truncated in the conversation item fed back to the model.
func TestRunnerCapsModelFacingToolOutput(t *testing.T) {
	huge := strings.Repeat("a", 3000) + "TAIL-MARKER"
	bigTool := &FunctionTool{
		ToolName:        "big",
		ToolDescription: "returns huge output",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			return huge, nil
		},
	}
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "big", Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{bigTool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{MaxToolOutputBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var toolOutput string
	for _, item := range result.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID == "c1" {
			toolOutput = item.ToolOutput.Content
		}
	}
	if len(toolOutput) > 1100 {
		t.Fatalf("tool output not capped: len=%d", len(toolOutput))
	}
	if !strings.Contains(toolOutput, "elided") {
		t.Fatalf("expected elision marker in capped output: %q", toolOutput[:80])
	}
	if !strings.HasSuffix(toolOutput, "TAIL-MARKER") {
		t.Fatalf("middle truncation should preserve the tail")
	}

	// Negative value disables the cap.
	model2 := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "big", Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	result2, err := NewRunnerWithModel(model2).Run(context.Background(), agent, nil, RunConfig{MaxToolOutputBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result2.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID == "c1" {
			if item.ToolOutput.Content != huge {
				t.Fatal("cap disabled but output was truncated")
			}
		}
	}
}

// TestAgentToolInheritsParentToolPolicy verifies that the sync agent-as-tool
// path forwards the parent run's ToolPolicy and access level into nested runs.
func TestAgentToolInheritsParentToolPolicy(t *testing.T) {
	mutatingTool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "writes things",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			return "mutated", nil
		},
	}
	model := &mockModel{
		responses: []*ModelResponse{
			// Parent turn: call sub-agent.
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "agent_worker", Input: json.RawMessage(`{"message":"go"}`)}}}},
			// Child turn: call mutating tool — must hit the approval pause.
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "mutate", Input: json.RawMessage(`{}`)}}}},
			// Child final (only reached if approval was not enforced).
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "child done"}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "parent done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	worker := &Agent{Name: "worker", Tools: []Tool{mutatingTool}}
	parent := &Agent{Name: "parent", Tools: []Tool{worker.AsTool(runner)}}

	result, err := runner.Run(context.Background(), parent, nil, RunConfig{
		ToolPolicy: &ToolPolicy{ApprovalRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The nested run inherits ApprovalRequired, so the child's mutating call
	// pauses with an interruption; its run result reports "stopped" rather
	// than executing the tool.
	for _, item := range result.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.Content == "mutated" {
			t.Fatal("nested mutating tool executed despite parent ApprovalRequired policy")
		}
	}
}

// TestRequireCompletionConfirmation verifies the first final answer is bounced
// back with a verification prompt and the second consecutive one finalizes.
func TestRequireCompletionConfirmation(t *testing.T) {
	echoTool := &FunctionTool{
		ToolName:        "check",
		ToolDescription: "verification tool",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			return "verified", nil
		},
	}
	model := &mockModel{
		responses: []*ModelResponse{
			// First final answer — should be bounced.
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "premature answer"}}}},
			// Model verifies with a tool call — resets pendingCompletion.
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "check", Input: json.RawMessage(`{}`)}}}},
			// Final answer again — bounced once more (streak was reset).
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "verified answer"}}}},
			// Confirmed final answer.
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "confirmed final"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{echoTool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{RequireCompletionConfirmation: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "confirmed final" {
		t.Fatalf("expected confirmed final answer, got %q", result.FinalText())
	}
	confirmations := 0
	for _, item := range result.NewItems {
		if item.Type == RunItemMessage && item.Message != nil && strings.Contains(item.Message.Text, "verify your work now") {
			confirmations++
		}
	}
	if confirmations != 2 {
		t.Fatalf("expected 2 confirmation bounces, got %d", confirmations)
	}
}

// TestRequireCompletionConfirmationOffByDefault verifies default behavior is
// unchanged.
func TestRequireCompletionConfirmationOffByDefault(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "immediate answer"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}
	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "immediate answer" {
		t.Fatalf("got %q", result.FinalText())
	}
}

// TestPlanRecitationAppendedTransiently verifies the plan is recited as the
// last input item of each request without being persisted to history.
func TestPlanRecitationAppendedTransiently(t *testing.T) {
	tool := &FunctionTool{
		ToolName:        "noop",
		ToolDescription: "noop",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn:              func(ctx context.Context, _ json.RawMessage) (string, error) { return "ok", nil },
	}
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "noop", Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{tool}}

	calls := 0
	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		PlanRecitation: func(context.Context) string {
			calls++
			return "1. do the thing (in progress)"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("recitation should run once per model call, got %d", calls)
	}
	for i, req := range model.requests {
		last := req.Input[len(req.Input)-1]
		if last.Type != RunItemMessage || last.Message == nil || !strings.Contains(last.Message.Text, "<current_plan>") {
			t.Fatalf("request %d: last input item should be the plan recitation", i)
		}
		// Recitation must appear exactly once per request (not accumulate).
		count := 0
		for _, item := range req.Input {
			if item.Type == RunItemMessage && item.Message != nil && strings.Contains(item.Message.Text, "<current_plan>") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("request %d: expected 1 recitation, got %d", i, count)
		}
	}
	// Not persisted to history.
	for _, item := range result.NewItems {
		if item.Type == RunItemMessage && item.Message != nil && strings.Contains(item.Message.Text, "<current_plan>") {
			t.Fatal("plan recitation must not be persisted to run history")
		}
	}
}
