package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// --- RetryPolicy: failures on later turns must still be retried -------------

// Regression test: llmAttempt accumulates across the whole run, but the retry
// budget must apply per failing call site. A transient failure on a turn later
// than MaxRetries previously aborted the run without a single retry.
func TestRunnerRetryPolicyRetriesFailuresOnLaterTurns(t *testing.T) {
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echo",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "ok", nil
		},
	}
	// Three successful tool-call turns, then one transient failure, then recovery.
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "1", Name: "echo", Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "2", Name: "echo", Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "3", Name: "echo", Input: json.RawMessage(`{}`)}}}},
			nil, // transient failure on the 4th model call
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "recovered"}}}},
		},
		errors: []error{nil, nil, nil, errors.New("temporary outage"), nil},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{echoTool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns: 10,
		RetryPolicy: &RetryPolicy{
			MaxRetries: 3,
			Backoff:    RetryBackoffSettings{InitialDelayMS: 0, MaxDelayMS: 0, Multiplier: 1},
		},
	})
	if err != nil {
		t.Fatalf("expected retry to recover, got error: %v", err)
	}
	if result.FinalText() != "recovered" {
		t.Fatalf("FinalText() = %q, want recovered", result.FinalText())
	}
	if model.callIdx != 5 {
		t.Fatalf("model calls = %d, want 5", model.callIdx)
	}
}

// The consecutive-failure counter must reset on success so the retry budget is
// not shared between unrelated failures, but consecutive failures past the
// budget still abort.
func TestRunnerRetryPolicyConsecutiveFailuresExhaustBudget(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{nil, nil},
		errors:    []error{errors.New("outage 1"), errors.New("outage 2")},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	_, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns: 10,
		RetryPolicy: &RetryPolicy{
			MaxRetries: 1,
			Backoff:    RetryBackoffSettings{InitialDelayMS: 0, MaxDelayMS: 0, Multiplier: 1},
		},
	})
	if err == nil {
		t.Fatal("expected error after retry budget exhausted")
	}
	if model.callIdx != 2 {
		t.Fatalf("model calls = %d, want 2 (initial + 1 retry)", model.callIdx)
	}
}

// --- Model name prefix stripping --------------------------------------------

// recordingProvider returns one shared request-recording model for any name.
type recordingProvider struct {
	model *mockModel
}

func (p *recordingProvider) GetModel(name string) (Model, error) { return p.model, nil }
func (p *recordingProvider) Close() error                        { return nil }

// Model IDs that contain "/" as part of the ID (e.g. OpenRouter
// "vendor/model") must reach the API request intact when the provider is not
// prefix-aware and the prefix does not name the provider.
func TestRunnerPreservesSlashModelIDsForPlainProviders(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "hi"}}}},
		},
	}
	runner := NewRunnerWithProvider(&recordingProvider{model: model})
	agent := &Agent{Name: "test", Model: "anthropic/claude-opus-4"}

	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{MaxTurns: 1}); err != nil {
		t.Fatal(err)
	}
	if got := model.requests[0].Model; got != "anthropic/claude-opus-4" {
		t.Fatalf("request model = %q, want %q (prefix must not be stripped)", got, "anthropic/claude-opus-4")
	}
}

// A prefix that names the provider itself is still stripped for plain
// providers (the documented "openai/gpt-5.4" → "gpt-5.4" behavior).
func TestRunnerStripsOwnProviderPrefixForPlainProviders(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "hi"}}}},
		},
	}
	runner := NewRunnerWithProvider(&recordingProvider{model: model})
	agent := &Agent{Name: "test", Model: "mock/some-model"} // mockModel.Provider() == "mock"

	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{MaxTurns: 1}); err != nil {
		t.Fatal(err)
	}
	if got := model.requests[0].Model; got != "some-model" {
		t.Fatalf("request model = %q, want %q", got, "some-model")
	}
}

func TestMultiProviderNormalizeModelName(t *testing.T) {
	mp := NewMultiProvider("openai")
	mp.Register("openai", &stubProvider{name: "openai"})
	mp.Register("openrouter", &stubProvider{name: "openrouter"})

	tests := []struct {
		in   string
		want string
	}{
		{"openai/gpt-5.4", "gpt-5.4"},                           // registered prefix stripped
		{"openrouter/anthropic/claude-x", "anthropic/claude-x"}, // only the routing prefix stripped
		{"anthropic/claude-x", "anthropic/claude-x"},            // unregistered prefix preserved
		{"gpt-5.4", "gpt-5.4"},                                  // no prefix
		{"", ""},                                                // empty
	}
	for _, tt := range tests {
		if got := mp.NormalizeModelName(tt.in); got != tt.want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- Nested runs inherit tool guardrails ------------------------------------

// guardrailToolRecorder returns a tool input guardrail that records the names
// of tools it inspects.
func guardrailToolRecorder(mu *sync.Mutex, seen map[string]int) ToolInputGuardrail {
	return ToolInputGuardrail{
		Name: "recorder",
		Fn: func(_ *RunContext, _ *Agent, tool Tool, _ json.RawMessage) (*GuardrailResult, error) {
			mu.Lock()
			defer mu.Unlock()
			seen[tool.Name()]++
			return &GuardrailResult{}, nil
		},
	}
}

// Agent.AsTool child runs must apply the parent run's tool input guardrails to
// the child's own tool calls.
func TestAgentAsToolInheritsToolInputGuardrails(t *testing.T) {
	childTool := &FunctionTool{
		ToolName:        "child_tool",
		ToolDescription: "child tool",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "child ok", nil
		},
	}
	// Shared model script: parent calls agent tool → child calls child_tool →
	// child finishes → parent finishes.
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "p1", Name: "agent_child", Input: json.RawMessage(`{"message":"go"}`)}}}},
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "child_tool", Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "child done"}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "parent done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	child := &Agent{Name: "child", Tools: []Tool{childTool}}
	parent := &Agent{Name: "parent", Tools: []Tool{child.AsTool(runner)}}

	var mu sync.Mutex
	seen := map[string]int{}
	result, err := runner.Run(context.Background(), parent, nil, RunConfig{
		MaxTurns:            5,
		ToolInputGuardrails: []ToolInputGuardrail{guardrailToolRecorder(&mu, seen)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "parent done" {
		t.Fatalf("FinalText() = %q, want parent done", result.FinalText())
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["child_tool"] == 0 {
		t.Fatalf("tool input guardrail did not run for nested child tool call; seen=%v", seen)
	}
}

// Async sub-agent tasks spawned from within a run must inherit that run's tool
// input guardrails via the nested-run context.
func TestSubAgentRegistryTaskInheritsToolInputGuardrails(t *testing.T) {
	childTool := &FunctionTool{
		ToolName:        "child_tool",
		ToolDescription: "child tool",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "child ok", nil
		},
	}
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "child_tool", Input: json.RawMessage(`{}`)}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "child done"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{
		Runner: runner,
		Agents: map[string]*Agent{"child": {Name: "child", Tools: []Tool{childTool}}},
	})

	var mu sync.Mutex
	seen := map[string]int{}
	spawnCtx := WithNestedRunConfig(context.Background(), RunConfig{
		ToolInputGuardrails: []ToolInputGuardrail{guardrailToolRecorder(&mu, seen)},
	})
	taskID, err := registry.SpawnAsync(spawnCtx, "child", "go", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := registry.WaitForTask(context.Background(), taskID, int64((5 * time.Second).Milliseconds()))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != SubAgentTaskCompleted {
		t.Fatalf("task status = %q, want completed (error: %s)", task.Status, task.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["child_tool"] == 0 {
		t.Fatalf("tool input guardrail did not run for sub-agent task tool call; seen=%v", seen)
	}
}
