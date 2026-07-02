package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// mockModel is a test Model that returns canned responses.
type mockModel struct {
	responses    []*ModelResponse
	errors       []error
	retryAdvices []ModelRetryAdvice
	callIdx      int
	costPerCall  float64
	requests     []ModelRequest
}

func (m *mockModel) GetResponse(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.requests = append(m.requests, req)
	idx := m.callIdx
	m.callIdx++
	if idx < len(m.errors) && m.errors[idx] != nil {
		return nil, m.errors[idx]
	}
	if idx >= len(m.responses) {
		return nil, errors.New("no more responses")
	}
	return m.responses[idx], nil
}

func (m *mockModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	resp, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan ModelStreamEvent, len(resp.Items)+1)
	done := make(chan *ModelResponse, 1)
	go func() {
		defer close(events)
		for i := range resp.Items {
			item := resp.Items[i]
			if item.Type == RunItemMessage && item.Message != nil {
				events <- ModelStreamEvent{Type: ModelStreamDelta, Delta: item.Message.Text}
			}
			events <- ModelStreamEvent{Type: ModelStreamItemDone, Item: &item}
		}
		events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
		done <- resp
	}()
	return NewModelStream(events, done), nil
}

func (m *mockModel) GetRetryAdvice(_ error) *ModelRetryAdvice {
	idx := m.callIdx - 1
	if idx >= 0 && idx < len(m.retryAdvices) {
		advice := m.retryAdvices[idx]
		return &advice
	}
	return &ModelRetryAdvice{ShouldRetry: false}
}

func (m *mockModel) CalculateCost(_ Usage) float64 {
	return m.costPerCall
}

func (m *mockModel) Provider() string { return "mock" }

func TestRunnerSimpleTextResponse(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "Hello, world!"}},
				},
				Usage: Usage{InputTokens: 10, OutputTokens: 5},
			},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Instructions: "Be helpful"}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "Hello, world!" {
		t.Errorf("expected 'Hello, world!', got %q", result.FinalText())
	}
	if result.LastAgent != agent {
		t.Error("last agent should be the original agent")
	}
	if result.Usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", result.Usage.InputTokens)
	}
}

func TestRunStreamedEmitsSubAgentStreamEvents(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "call-subagent",
				Name:  "agent_worker",
				Input: json.RawMessage(`{"message":"do delegated work"}`),
			}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "child done"}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "final"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	var eventBytes bytes.Buffer
	hooks := NewPlatformHooks(NewProgressTracker(), NewEventStream(&eventBytes))
	runner.DefaultHooks = hooks

	worker := &Agent{Name: "worker"}
	parent := &Agent{Name: "parent", Tools: []Tool{worker.AsTool(runner)}}
	streamed := runner.RunStreamed(context.Background(), parent, nil, RunConfig{
		Hooks:    hooks,
		MaxTurns: 4,
	})

	var statuses []string
	completedAgent := ""
	for event := range streamed.Events {
		if event.Type != StreamEventSubAgent {
			continue
		}
		if event.Content == nil || event.SubAgent == nil {
			t.Fatalf("subagent stream event missing payload: %+v", event)
		}
		statuses = append(statuses, event.SubAgent.Status)
		if event.SubAgent.Status == "completed" {
			completedAgent = event.SubAgent.AgentName
		}
	}
	result := streamed.FinalResult()
	if err := streamed.Err(); err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "final" {
		t.Fatalf("FinalText() = %q, want final", result.FinalText())
	}
	if !containsString(statuses, "started") || !containsString(statuses, "completed") {
		t.Fatalf("subagent statuses = %v, want started and completed", statuses)
	}
	if completedAgent != "worker" {
		t.Fatalf("completed subagent agent = %q, want worker", completedAgent)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRunnerToolCallAndResponse(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "call1", Name: "echo", Input: json.RawMessage(`"hello"`),
					}},
				},
				Usage: Usage{InputTokens: 10, OutputTokens: 5},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "Tool said: hello"}},
				},
				Usage: Usage{InputTokens: 15, OutputTokens: 8},
			},
		},
	}

	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			return "echoed: " + string(input), nil
		},
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{echoTool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "Tool said: hello" {
		t.Errorf("expected 'Tool said: hello', got %q", result.FinalText())
	}
	if result.Usage.InputTokens != 25 {
		t.Errorf("expected 25 total input tokens, got %d", result.Usage.InputTokens)
	}
}

func TestRunnerToolInputGuardrailTripwireReturnsToolErrorAndContinues(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID:    "call1",
						Name:  "mutate",
						Input: json.RawMessage(`{"value":"secret"}`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "guardrail handled"}},
				},
			},
		},
	}
	executed := false
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "mutates state",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "mutated", nil
		},
	}

	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test", Tools: []Tool{tool}}, nil, RunConfig{
		MaxTurns: 3,
		ToolInputGuardrails: []ToolInputGuardrail{{
			Name: "block-secret-input",
			Fn: func(_ *RunContext, _ *Agent, _ Tool, input json.RawMessage) (*GuardrailResult, error) {
				return &GuardrailResult{TripwireTriggered: strings.Contains(string(input), "secret")}, nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if executed {
		t.Fatal("tool executed despite input guardrail tripwire")
	}
	if result.FinalText() != "guardrail handled" {
		t.Fatalf("FinalText() = %q, want guardrail handled", result.FinalText())
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	if len(result.ToolInputGuardrailResults) != 1 || !result.ToolInputGuardrailResults[0].TripwireTriggered {
		t.Fatalf("ToolInputGuardrailResults = %#v, want one triggered result", result.ToolInputGuardrailResults)
	}
	if !runItemsContainToolOutput(result.NewItems, "tool input guardrail") {
		t.Fatalf("NewItems = %#v, want guarded tool error output", result.NewItems)
	}
}

func TestRunnerToolOutputGuardrailTripwireReturnsToolErrorAndRedactsTrace(t *testing.T) {
	const secretOutput = "SECRET_TOKEN_VALUE"
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID:    "call1",
						Name:  "read_secret",
						Input: json.RawMessage(`{"path":"secret.txt"}`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "guardrail handled"}},
				},
			},
		},
	}
	tool := &FunctionTool{
		ToolName:        "read_secret",
		ToolDescription: "returns secret content",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return secretOutput, nil
		},
	}
	tracing := &captureTracingProcessor{}

	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test", Tools: []Tool{tool}}, nil, RunConfig{
		MaxTurns:         3,
		TracingProcessor: tracing,
		ToolOutputGuardrails: []ToolOutputGuardrail{{
			Name: "block-secret-output",
			Fn: func(_ *RunContext, _ *Agent, _ Tool, result ToolResult) (*GuardrailResult, error) {
				return &GuardrailResult{TripwireTriggered: strings.Contains(result.Content, secretOutput)}, nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.FinalText() != "guardrail handled" {
		t.Fatalf("FinalText() = %q, want guardrail handled", result.FinalText())
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	if len(result.ToolOutputGuardrailResults) != 1 || !result.ToolOutputGuardrailResults[0].TripwireTriggered {
		t.Fatalf("ToolOutputGuardrailResults = %#v, want one triggered result", result.ToolOutputGuardrailResults)
	}
	if !runItemsContainToolOutput(result.NewItems, "tool output guardrail") {
		t.Fatalf("NewItems = %#v, want guarded tool error output", result.NewItems)
	}
	if runItemsContainText(result.NewItems, secretOutput) {
		t.Fatalf("NewItems leaked blocked tool output: %#v", result.NewItems)
	}
	if runItemsContainText(model.requests[1].Input, secretOutput) {
		t.Fatalf("second model input leaked blocked tool output: %#v", model.requests[1].Input)
	}
	if tracing.functionOutputContains(secretOutput) {
		t.Fatalf("function span leaked blocked tool output: %#v", tracing.spans)
	}
}

func TestRunnerReadOnlyAccessUsesAdaptedToolForRequestAndExecution(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "call1", Name: "Bash", Input: json.RawMessage(`{}`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}},
				},
			},
		},
	}
	tool := &accessAdaptingTool{}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{tool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		ToolAccessLevel: ToolAccessLevelReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) == 0 || len(model.requests[0].Tools) != 1 || model.requests[0].Tools[0].Name() != "Bash" {
		t.Fatalf("read-only request tools = %+v, want adapted Bash", model.requests[0].Tools)
	}
	if !model.requests[0].Tools[0].IsReadOnly() {
		t.Fatalf("adapted request tool should be read-only")
	}

	var gotOutput string
	for _, item := range result.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID == "call1" {
			gotOutput = item.ToolOutput.Content
		}
	}
	if gotOutput != "read-only bash" {
		t.Fatalf("tool output = %q, want read-only bash", gotOutput)
	}
	if tool.executed {
		t.Fatalf("original write-capable tool executed in read-only run")
	}
}

func TestRunnerHonorsToolIsEnabled(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
		}},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{disabledTestTool{}}}

	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(model.requests))
	}
	if len(model.requests[0].Tools) != 0 {
		t.Fatalf("request tools = %#v, want disabled tool filtered out", model.requests[0].Tools)
	}
}

type disabledTestTool struct{}

func (disabledTestTool) Name() string                 { return "disabled" }
func (disabledTestTool) Description() string          { return "disabled tool" }
func (disabledTestTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (disabledTestTool) IsReadOnly() bool             { return true }
func (disabledTestTool) IsEnabled(*RunContext) bool   { return false }
func (disabledTestTool) NeedsApproval() bool          { return false }
func (disabledTestTool) TimeoutSeconds() int          { return 0 }
func (disabledTestTool) Execute(context.Context, json.RawMessage, string) (ToolResult, error) {
	return ToolResult{Content: "should not execute"}, nil
}

func TestRunnerBuildsPortablePromptContext(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: `{"answer":"ok"}`}},
				},
			},
		},
	}
	schema := NewOutputSchema("answer", json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`))
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:         "test",
		Instructions: "base instructions",
		OutputType:   schema,
		MCPServers:   []string{"filesystem", "github"},
	}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns:               1,
		AdditionalInstructions: "extra run instructions",
	})
	if err != nil {
		t.Fatal(err)
	}
	output, ok := result.FinalOutput.(map[string]any)
	if !ok || output["answer"] != "ok" {
		t.Fatalf("FinalOutput = %#v, want parsed JSON object", result.FinalOutput)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	got := model.requests[0].Instructions
	for _, want := range []string{
		"base instructions",
		"extra run instructions",
		"<structured_output>",
		`"answer"`,
		"Connected MCP servers: filesystem, github",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions missing %q:\n%s", want, got)
		}
	}
	if model.requests[0].OutputSchema == nil || model.requests[0].OutputSchema.Name != "answer" {
		t.Fatalf("OutputSchema = %#v, want answer schema", model.requests[0].OutputSchema)
	}
}

func TestRunnerModeInstructionsFallback(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}}}}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Instructions: "base"}

	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{MaxTurns: 1, ModeInstructions: "legacy mode"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.requests[0].Instructions, "legacy mode") {
		t.Fatalf("instructions = %q, want legacy mode instructions", model.requests[0].Instructions)
	}
}

func TestRunnerToolPolicyRequiresApproval(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID:    "call1",
						Name:  "mutate",
						Input: json.RawMessage(`{"value":"x"}`),
					}},
				},
			},
		},
	}
	executed := false
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "writes something",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "wrote", nil
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{tool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		ToolPolicy: &ToolPolicy{ApprovalRequired: true, DefaultTimeout: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Interruption == nil || result.Interruption.ToolName != "mutate" {
		t.Fatalf("interruption = %#v, want mutate approval", result.Interruption)
	}
	if executed {
		t.Fatal("tool executed despite approval policy")
	}
	if len(model.requests) != 1 || len(model.requests[0].Tools) != 1 {
		t.Fatalf("request tools = %#v, want one tool", model.requests)
	}
	if !model.requests[0].Tools[0].NeedsApproval() {
		t.Fatal("request tool should require approval")
	}
	if got := model.requests[0].Tools[0].TimeoutSeconds(); got != 7 {
		t.Fatalf("request tool timeout = %d, want 7", got)
	}
}

func TestRunnerStopAtToolsUsesToolOutputAsFinal(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID:    "call1",
						Name:  "echo",
						Input: json.RawMessage(`{"text":"hello"}`),
					}},
				},
			},
		},
	}
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "hello from tool", nil
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:               "test",
		Tools:              []Tool{echoTool},
		StopAtTools:        &StopAtTools{ToolNames: []string{"echo"}},
		ToolsToFinalOutput: &ToolsToFinalOutputResult{IsFinalOutput: true},
	}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "hello from tool" {
		t.Fatalf("FinalText() = %q, want tool output", result.FinalText())
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
}

func TestRunnerStopAtToolsRunsOutputGuardrails(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{{
			Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "call1",
				Name:  "echo",
				Input: json.RawMessage(`{"text":"hello"}`),
			}}},
		}},
	}
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "blocked tool final output", nil
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:               "test",
		Tools:              []Tool{echoTool},
		StopAtTools:        &StopAtTools{ToolNames: []string{"echo"}},
		ToolsToFinalOutput: &ToolsToFinalOutputResult{IsFinalOutput: true},
		OutputGuardrails: []OutputGuardrail{{
			Name: "block-tool-final",
			Fn: func(_ *RunContext, _ *Agent, output any) (*GuardrailResult, error) {
				if strings.Contains(fmt.Sprint(output), "blocked") {
					return &GuardrailResult{TripwireTriggered: true, Output: "blocked"}, nil
				}
				return &GuardrailResult{}, nil
			},
		}},
	}

	_, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err == nil {
		t.Fatal("expected output guardrail error")
	}
	var tripwire *OutputGuardrailTripwireTriggered
	if !errors.As(err, &tripwire) {
		t.Fatalf("expected OutputGuardrailTripwireTriggered, got %T: %v", err, err)
	}
}

func TestRunnerRetryPolicyRetriesModelErrors(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			nil,
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "recovered"}}}},
		},
		errors: []error{errors.New("temporary outage")},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns: 2,
		RetryPolicy: &RetryPolicy{
			MaxRetries: 1,
			Backoff: RetryBackoffSettings{
				InitialDelayMS: 0,
				MaxDelayMS:     0,
				Multiplier:     1,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "recovered" {
		t.Fatalf("FinalText() = %q, want recovered", result.FinalText())
	}
	if model.callIdx != 2 {
		t.Fatalf("model calls = %d, want 2", model.callIdx)
	}
}

func TestRunnerRetryPolicyDoesNotRetryContextCancellation(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			nil,
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "should not run"}}}},
		},
		errors: []error{context.Canceled},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	_, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns:    2,
		RetryPolicy: &RetryPolicy{MaxRetries: 3},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if model.callIdx != 1 {
		t.Fatalf("model calls = %d, want 1", model.callIdx)
	}
}

func TestRunnerRetryPolicyHonorsNonRetryableAdvice(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			nil,
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "should not run"}}}},
		},
		errors:       []error{errors.New("bad request")},
		retryAdvices: []ModelRetryAdvice{{ShouldRetry: false, Reason: "400"}},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	_, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns:    2,
		RetryPolicy: &RetryPolicy{MaxRetries: 3},
	})
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("error = %v, want original failure", err)
	}
	if model.callIdx != 1 {
		t.Fatalf("model calls = %d, want 1", model.callIdx)
	}
}

func TestAgentAsToolSanitizesToolName(t *testing.T) {
	tool := (&Agent{Name: "Code Reviewer/QA"}).AsTool(NewRunnerWithModel(&mockModel{}))
	if tool.Name() != "agent_Code_Reviewer_QA" {
		t.Fatalf("tool name = %q, want sanitized agent tool name", tool.Name())
	}
}

func TestRunnerErrorHandlerAbortOverridesRetryPolicy(t *testing.T) {
	model := &mockModel{
		errors: []error{errors.New("do not retry")},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	_, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns: 2,
		ErrorHandler: func(RunErrorData) RunErrorHandlerResult {
			return RunErrorHandlerResult{Action: ErrorActionAbort}
		},
		RetryPolicy: &RetryPolicy{
			MaxRetries: 3,
			Backoff: RetryBackoffSettings{
				InitialDelayMS: 0,
				MaxDelayMS:     0,
				Multiplier:     1,
			},
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want abort error")
	}
	if model.callIdx != 1 {
		t.Fatalf("model calls = %d, want no retry after abort", model.callIdx)
	}
}

func TestRunnerRunStreamedUsesModelStreamDeltas(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "hello stream"}}}},
		},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	streamed := runner.RunStreamed(context.Background(), agent, nil, RunConfig{MaxTurns: 1})
	var delta string
	var items int
	for event := range streamed.Events {
		switch event.Type {
		case StreamEventRawResponse:
			delta += event.Delta
		case StreamEventRunItem:
			items++
		}
	}
	result := streamed.FinalResult()
	if result.FinalText() != "hello stream" {
		t.Fatalf("FinalText() = %q, want hello stream", result.FinalText())
	}
	if delta != "hello stream" {
		t.Fatalf("delta = %q, want hello stream", delta)
	}
	if items != 1 {
		t.Fatalf("run item events = %d, want 1", items)
	}
}

func TestRunnerMaxTurnsExceeded(t *testing.T) {
	model := &mockModel{
		responses: make([]*ModelResponse, 10),
	}
	for i := range model.responses {
		model.responses[i] = &ModelResponse{
			Items: []RunItem{
				{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID: "call", Name: "echo", Input: json.RawMessage(`"loop"`),
				}},
			},
		}
	}

	echoTool := &FunctionTool{
		ToolName: "echo", ToolDescription: "echo",
		Schema: json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{echoTool}}

	_, err := runner.Run(context.Background(), agent, nil, RunConfig{MaxTurns: 3})
	if err == nil {
		t.Fatal("expected MaxTurnsExceeded error")
	}
	var maxTurns *MaxTurnsExceeded
	if !errors.As(err, &maxTurns) {
		t.Fatalf("expected MaxTurnsExceeded, got %T: %v", err, err)
	}
	if maxTurns.MaxTurns != 3 {
		t.Errorf("expected max turns 3, got %d", maxTurns.MaxTurns)
	}
}

type accessAdaptingTool struct {
	executed bool
}

func (t *accessAdaptingTool) Name() string        { return "Bash" }
func (t *accessAdaptingTool) Description() string { return "write-capable bash" }
func (t *accessAdaptingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *accessAdaptingTool) IsReadOnly() bool           { return false }
func (t *accessAdaptingTool) IsEnabled(*RunContext) bool { return true }
func (t *accessAdaptingTool) NeedsApproval() bool        { return false }
func (t *accessAdaptingTool) TimeoutSeconds() int        { return 0 }
func (t *accessAdaptingTool) Execute(context.Context, json.RawMessage, string) (ToolResult, error) {
	t.executed = true
	return ToolResult{Content: "write bash"}, nil
}
func (t *accessAdaptingTool) ToolForAccess(level ToolAccessLevel) Tool {
	if level != ToolAccessLevelReadOnly {
		return t
	}
	return &FunctionTool{
		ToolName:        "Bash",
		ToolDescription: "read-only bash",
		Schema:          json.RawMessage(`{"type":"object"}`),
		ReadOnly:        true,
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "read-only bash", nil
		},
	}
}

func TestAgentToolUsesNestedSubAgentMaxTurns(t *testing.T) {
	model := &mockModel{
		responses: make([]*ModelResponse, 10),
	}
	for i := range model.responses {
		model.responses[i] = &ModelResponse{
			Items: []RunItem{
				{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID: "call", Name: "echo", Input: json.RawMessage(`"loop"`),
				}},
			},
		}
	}

	echoTool := &FunctionTool{
		ToolName: "echo", ToolDescription: "echo",
		Schema: json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	runner := NewRunnerWithModel(model)
	helper := &Agent{Name: "helper", Tools: []Tool{echoTool}}
	tool := helper.AsTool(runner)
	ctx := WithNestedRunConfig(context.Background(), RunConfig{
		MaxTurns:         400,
		SubAgentMaxTurns: 2,
	})

	result, err := tool.Execute(ctx, json.RawMessage(`{"message":"loop"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected agent tool result to be an error")
	}
	if !strings.Contains(result.Content, "max turns exceeded: 2") {
		t.Fatalf("result content = %q, want max turns exceeded: 2", result.Content)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
}

func TestRunnerForceFinalSummaryTurn(t *testing.T) {
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
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "partial summary"}}},
			},
		},
	}
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echo",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Instructions: "investigate", Tools: []Tool{echoTool}}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{
		MaxTurns:              3,
		ForceFinalSummaryTurn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "partial summary" {
		t.Fatalf("final text = %q, want partial summary", result.FinalText())
	}
	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(model.requests))
	}
	if len(model.requests[2].Tools) != 0 {
		t.Fatalf("final request exposed %d tools, want 0", len(model.requests[2].Tools))
	}
	if !strings.Contains(model.requests[2].Instructions, "This is your final available turn") {
		t.Fatalf("final request instructions missing final-turn directive: %q", model.requests[2].Instructions)
	}
}

func TestRunnerHandoff(t *testing.T) {
	expertAgent := &Agent{Name: "expert", Instructions: "Expert agent"}

	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "h1", Name: "transfer_to_expert", Input: json.RawMessage(`{}`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "Expert response"}},
				},
			},
		},
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:     "router",
		Handoffs: []*Handoff{NewHandoff(expertAgent)},
	}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "Expert response" {
		t.Errorf("expected 'Expert response', got %q", result.FinalText())
	}
	if result.LastAgent != expertAgent {
		t.Error("last agent should be the expert agent after handoff")
	}
}

func TestRunnerEmitsSpawnedSubagentLifecycleForAgentTool(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "spawn_1", Name: "agent_helper", Input: json.RawMessage(`{"message":"Inspect the flaky test"}`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "Nested helper result"}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "Parent done"}},
				},
			},
		},
	}

	tracker := NewProgressTracker()
	tracker.SetSession(1, "implementing")
	hooks := NewPlatformHooks(tracker, nil)
	hooks.Turn = 1

	runner := NewRunnerWithModel(model)
	runner.DefaultHooks = hooks

	helper := &Agent{Name: "helper", Instructions: "Nested helper"}
	parent := &Agent{Name: "parent", Tools: []Tool{helper.AsTool(runner)}}

	result, err := runner.Run(context.Background(), parent, nil, RunConfig{Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "Parent done" {
		t.Fatalf("final text = %q, want Parent done", result.FinalText())
	}

	snap := tracker.Snapshot()
	if snap.AgentCount != 1 {
		t.Fatalf("AgentCount = %d, want 1", snap.AgentCount)
	}
}

func TestAgentToolInheritsCompactionConfig(t *testing.T) {
	longOutput := strings.Repeat("nested tool output that should be compacted ", 20)
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID:    "spawn-architect",
					Name:  "agent_architect",
					Input: json.RawMessage(`{"message":"Design this change"}`),
				}}},
			},
			{
				Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID:    "nested-call-1",
					Name:  "echo",
					Input: json.RawMessage(`{"n":1}`),
				}}},
			},
			{
				Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
					ID:    "nested-call-2",
					Name:  "echo",
					Input: json.RawMessage(`{"n":2}`),
				}}},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "architect done"}}},
			},
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "parent done"}}},
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
	architect := &Agent{Name: "architect", Tools: []Tool{echoTool}}
	parent := &Agent{Name: "parent", Tools: []Tool{architect.AsTool(runner)}}

	result, err := runner.Run(context.Background(), parent, nil, RunConfig{
		CompactionConfig: CompactionConfig{
			Enabled:                     true,
			TriggerTokens:               10,
			TargetTokens:                20,
			PreserveRecentItems:         1,
			PreserveInitialUserMessages: 1,
			SummaryBulletLimit:          1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "parent done" {
		t.Fatalf("final text = %q, want parent done", result.FinalText())
	}
	if len(model.requests) != 5 {
		t.Fatalf("model requests = %d, want 5", len(model.requests))
	}
	nestedThirdTurnInput := Items.ExtractText(model.requests[3].Input)
	if !strings.Contains(nestedThirdTurnInput, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("nested third turn input = %q, want compacted history summary", nestedThirdTurnInput)
	}
}

func TestRunnerInputGuardrailTripwire(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
		},
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name: "test",
		InputGuardrails: []InputGuardrail{
			{
				Name: "block-bad",
				Fn: func(_ *RunContext, _ *Agent, _ []RunItem) (*GuardrailResult, error) {
					return &GuardrailResult{TripwireTriggered: true, Output: "blocked"}, nil
				},
			},
		},
	}

	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "bad input"}}}
	_, err := runner.Run(context.Background(), agent, input, RunConfig{})
	if err == nil {
		t.Fatal("expected guardrail error")
	}
	var tripwire *InputGuardrailTripwireTriggered
	if !errors.As(err, &tripwire) {
		t.Fatalf("expected InputGuardrailTripwireTriggered, got %T: %v", err, err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests = %d, want 0 because input guardrails block before model execution", len(model.requests))
	}
}

func TestRunnerStopOnFirstTool(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "c1", Name: "echo", Input: json.RawMessage(`"hi"`),
					}},
				},
			},
		},
	}

	echoTool := &FunctionTool{
		ToolName: "echo", ToolDescription: "echo",
		Schema: json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "done", nil
		},
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:            "test",
		Tools:           []Tool{echoTool},
		ToolUseBehavior: StopOnFirstTool,
	}

	result, err := runner.Run(context.Background(), agent, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalOutput != nil {
		t.Error("StopOnFirstTool should not have final output")
	}
	if len(result.NewItems) == 0 {
		t.Error("should have items from tool execution")
	}
}

func TestRunnerEmitsLLMAttemptEvents_SuccessFailureRetry(t *testing.T) {
	var buf strings.Builder
	es := NewEventStream(&buf)
	hooks := NewPlatformHooks(NewProgressTracker(), es)
	model := &mockModel{
		errors: []error{errors.New("boom"), nil},
		responses: []*ModelResponse{
			nil,
			{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
				Usage: Usage{InputTokens: 9, OutputTokens: 4, CacheReadTokens: 2, CacheCreateTokens: 1},
			},
		},
		retryAdvices: []ModelRetryAdvice{{ShouldRetry: true, RetryAfterMS: 0}},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Model: "openai/gpt-5.4"}
	res, err := runner.Run(context.Background(), agent, nil, RunConfig{Hooks: hooks, Phase: "implementing"})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText() != "done" {
		t.Fatalf("final text = %q", res.FinalText())
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var attempts []ContentEvent
	for _, line := range lines {
		var ev ContentEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "llm_attempt" {
			attempts = append(attempts, ev)
		}
	}
	if len(attempts) != 4 {
		t.Fatalf("expected 4 llm_attempt events, got %d", len(attempts))
	}
	if attempts[0].LLMAttempt != 1 || attempts[0].Status != "started" || attempts[0].AttemptStatus != "started" || attempts[0].LLMScope != "top_level" || attempts[0].RequestedModel != "openai/gpt-5.4" || attempts[0].ResolvedModel != "openai/gpt-5.4" || attempts[0].CanonicalModel != "mock/gpt-5.4" || attempts[0].Model != "mock/gpt-5.4" || attempts[0].Provider != "mock" {
		t.Fatalf("unexpected first attempt event: %+v", attempts[0])
	}
	if attempts[0].ToolUseID == "" {
		t.Fatal("first attempt missing ToolUseID")
	}
	if attempts[1].LLMAttempt != 1 || attempts[1].Status != "retrying" || attempts[1].AttemptStatus != "retrying" || attempts[1].ToolUseID != attempts[0].ToolUseID || !attempts[1].RetryPlanned {
		t.Fatalf("unexpected retry event: %+v", attempts[1])
	}
	if !attempts[1].IsError || attempts[1].Output != "boom" || attempts[1].FailureKind != "error" || attempts[1].Reason != "error" {
		t.Fatalf("retry event did not expose failure detail: %+v", attempts[1])
	}
	if attempts[2].LLMAttempt != 2 || attempts[2].Status != "started" || attempts[2].AttemptStatus != "started" {
		t.Fatalf("unexpected second start event: %+v", attempts[2])
	}
	if attempts[2].ToolUseID == "" || attempts[2].ToolUseID == attempts[0].ToolUseID {
		t.Fatalf("second attempt ToolUseID = %q, want distinct non-empty id", attempts[2].ToolUseID)
	}
	if attempts[3].LLMAttempt != 2 || attempts[3].Status != "completed" || attempts[3].AttemptStatus != "completed" || attempts[3].ToolUseID != attempts[2].ToolUseID {
		t.Fatalf("unexpected completion event: %+v", attempts[3])
	}
	if !attempts[3].UsageAvailable || attempts[3].InputTokens != 9 || attempts[3].OutputTokens != 4 || attempts[3].CacheReadInputTokens != 2 || attempts[3].CacheCreationInputTokens != 1 || attempts[3].TotalTokens != 13 {
		t.Fatalf("unexpected completion usage event: %+v", attempts[3])
	}
}

func TestRunnerEmitsLLMAttemptFailureEvent(t *testing.T) {
	var buf strings.Builder
	es := NewEventStream(&buf)
	hooks := NewPlatformHooks(NewProgressTracker(), es)
	model := &mockModel{
		errors: []error{context.DeadlineExceeded},
	}
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Model: "openai/gpt-5.4"}
	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{Hooks: hooks, Phase: "verifying"}); err == nil {
		t.Fatal("expected run error")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var attempts []ContentEvent
	for _, line := range lines {
		var ev ContentEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "llm_attempt" {
			attempts = append(attempts, ev)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 llm_attempt events, got %d", len(attempts))
	}
	if attempts[0].Status != "started" || attempts[1].Status != "failed" {
		t.Fatalf("unexpected llm attempt statuses: %+v", attempts)
	}
	if attempts[1].FailureKind != "deadline_exceeded" {
		t.Fatalf("FailureKind = %q, want deadline_exceeded", attempts[1].FailureKind)
	}
	if attempts[1].Reason != "deadline_exceeded" || !attempts[1].IsError || !strings.Contains(attempts[1].Output, "context deadline exceeded") {
		t.Fatalf("failure event did not expose provider error detail: %+v", attempts[1])
	}
	if attempts[1].ToolUseID == "" || attempts[1].ToolUseID != attempts[0].ToolUseID {
		t.Fatalf("failure attempt ids = %q / %q, want matching non-empty", attempts[0].ToolUseID, attempts[1].ToolUseID)
	}
}

func TestRunnerContextCancellation(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	_, err := runner.Run(ctx, agent, nil, RunConfig{})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestRunnerHooksAreCalled(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
		},
	}

	var hooksCalled []string
	hooks := &testRunHooks{
		onAgentStart: func(_ *RunContext, _ *Agent) { hooksCalled = append(hooksCalled, "agent_start") },
		onLLMStart:   func(_ *RunContext, _ *Agent) { hooksCalled = append(hooksCalled, "llm_start") },
		onLLMEnd:     func(_ *RunContext, _ *Agent, _ *ModelResponse) { hooksCalled = append(hooksCalled, "llm_end") },
		onAgentEnd:   func(_ *RunContext, _ *Agent, _ any) { hooksCalled = append(hooksCalled, "agent_end") },
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}

	_, err := runner.Run(context.Background(), agent, nil, RunConfig{Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{"agent_start", "llm_start", "llm_end", "agent_end"}
	if len(hooksCalled) != len(expected) {
		t.Fatalf("expected %d hooks, got %d: %v", len(expected), len(hooksCalled), hooksCalled)
	}
	for i, e := range expected {
		if hooksCalled[i] != e {
			t.Errorf("hook %d: expected %s, got %s", i, e, hooksCalled[i])
		}
	}
}

func TestRunnerImmediateInputPollerInjectsBeforeNextModelCall(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "call1", Name: "echo", Input: json.RawMessage(`"hello"`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}},
				},
			},
		},
	}

	echoTool := &FunctionTool{
		ToolName: "echo",
		Schema:   json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "echoed", nil
		},
	}

	pollerCalls := 0
	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test", Tools: []Tool{echoTool}}
	_, err := runner.Run(context.Background(), agent, nil, RunConfig{
		ImmediateInputPoller: func(context.Context) ([]RunItem, error) {
			pollerCalls++
			if pollerCalls == 2 {
				return []RunItem{{
					Type:    RunItemMessage,
					Message: &MessageOutput{Text: "interrupt now"},
				}}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	secondInput := Items.ExtractText(model.requests[1].Input)
	if secondInput == "" || !contains(secondInput, "interrupt now") {
		t.Fatalf("second request input = %q, want injected immediate message", secondInput)
	}
}

type finalJoinTool struct {
	items   []RunItem
	called  int
	pending bool
}

func (t *finalJoinTool) Name() string                 { return "subagent" }
func (t *finalJoinTool) Description() string          { return "test final join provider" }
func (t *finalJoinTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *finalJoinTool) Execute(context.Context, json.RawMessage, string) (ToolResult, error) {
	return ToolResult{Content: "unused"}, nil
}
func (t *finalJoinTool) IsReadOnly() bool           { return false }
func (t *finalJoinTool) IsEnabled(*RunContext) bool { return true }
func (t *finalJoinTool) NeedsApproval() bool        { return false }
func (t *finalJoinTool) TimeoutSeconds() int        { return 0 }
func (t *finalJoinTool) HasPendingSubAgentFinalJoin() bool {
	return t.pending
}
func (t *finalJoinTool) JoinSubAgentResults(context.Context) ([]RunItem, error) {
	t.called++
	if t.called > 1 {
		return nil, nil
	}
	t.pending = false
	return t.items, nil
}

func TestRunnerAutoJoinsSubAgentResultsBeforeFinal(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "premature final"}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "final with joined result"}}}},
		},
	}
	joinTool := &finalJoinTool{
		items: []RunItem{{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: "[SYSTEM] worker result ready"},
		}},
	}

	runner := NewRunnerWithModel(model)
	result, err := runner.Run(context.Background(), &Agent{Name: "test", Tools: []Tool{joinTool}}, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalOutput != "final with joined result" {
		t.Fatalf("FinalOutput = %q", result.FinalOutput)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	secondInput := Items.ExtractText(model.requests[1].Input)
	if !contains(secondInput, "premature final") || !contains(secondInput, "worker result ready") {
		t.Fatalf("second request input = %q, want premature final plus joined result", secondInput)
	}
}

func TestRunnerExtendsFinalTurnForSubAgentJoinItems(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "premature final"}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "final after child supervision"}}}},
		},
	}
	joinTool := &finalJoinTool{
		items: []RunItem{{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: "[SYSTEM] Managed sub-agent tasks are still active"},
		}},
	}

	runner := NewRunnerWithModel(model)
	result, err := runner.Run(context.Background(), &Agent{Name: "test", Tools: []Tool{joinTool}}, nil, RunConfig{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalOutput != "final after child supervision" {
		t.Fatalf("FinalOutput = %q", result.FinalOutput)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
}

func TestRunnerWaitsForSubAgentJoinAfterTurnBudget(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "call-status",
				Name:  "status",
				Input: json.RawMessage(`{}`),
			}}}},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "final after managed join"}}}},
		},
	}
	statusTool := &FunctionTool{
		ToolName: "status",
		Schema:   json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "still supervising", nil
		},
	}
	joinTool := &finalJoinTool{
		pending: true,
		items: []RunItem{{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: "[SYSTEM] Managed sub-agent task results are now available"},
		}},
	}

	runner := NewRunnerWithModel(model)
	result, err := runner.Run(context.Background(), &Agent{Name: "test", Tools: []Tool{statusTool, joinTool}}, nil, RunConfig{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalOutput != "final after managed join" {
		t.Fatalf("FinalOutput = %q", result.FinalOutput)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
}

func TestRunnerCompactionConfigSummarizesOlderHistory(t *testing.T) {
	longText := "this is a long user message that should trigger compaction because it keeps going and going and going"
	input := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: longText}},
		{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: longText}},
		{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: longText}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "keep the newest user context"}},
	}
	compacted, before, after, ok, _ := MaybeCompactRunItems(input, CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               10,
		TargetTokens:                200,
		PreserveRecentItems:         1,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          2,
	})
	if !ok {
		t.Fatal("expected compaction to occur")
	}
	if !(after < before) {
		t.Fatalf("tokens before=%d after=%d, want after < before", before, after)
	}
	text := Items.ExtractText(compacted)
	if !contains(text, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("compacted text = %q, want summary marker", text)
	}
	if !contains(text, "keep the newest user context") {
		t.Fatalf("compacted text = %q, want preserved tail context", text)
	}
}

func TestRunnerRequestAwareCompactionIncludesInstructionsOverhead(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}

	longText := strings.Repeat("request-aware compaction should include instructions and tools. ", 8)
	input := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "Original task"}},
	}
	for i := 0; i < 10; i++ {
		input = append(input, RunItem{
			Type:    RunItemMessage,
			Agent:   &Agent{Name: "assistant"},
			Message: &MessageOutput{Text: longText},
		})
	}
	beforeItems := estimateRunItemsTokens(input)

	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:         "test",
		Instructions: strings.Repeat("large mode instructions ", 40),
		Tools: []Tool{
			&FunctionTool{
				ToolName:        "large_tool",
				ToolDescription: strings.Repeat("large schema description ", 20),
				Schema:          json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
				Fn: func(context.Context, json.RawMessage) (string, error) {
					return "", nil
				},
			},
		},
	}

	_, err := runner.Run(context.Background(), agent, input, RunConfig{
		CompactionConfig: CompactionConfig{
			Enabled:                     true,
			TriggerTokens:               beforeItems + 10,
			TargetTokens:                100,
			PreserveRecentItems:         1,
			PreserveInitialUserMessages: 1,
			SummaryBulletLimit:          2,
		},
		ModelSettings: ModelSettings{MaxTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	gotText := Items.ExtractText(model.requests[0].Input)
	if !contains(gotText, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("request input = %q, want compacted history summary", gotText)
	}
}

func TestRunnerContextLengthErrorCompactsAndRetries(t *testing.T) {
	model := &mockModel{
		errors: []error{errors.New(`streaming response: {"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}`)},
		responses: []*ModelResponse{
			nil,
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "recovered"}}}},
		},
	}

	longText := strings.Repeat("history that must be forced compacted after provider rejection. ", 8)
	input := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "Original task"}},
	}
	for i := 0; i < 10; i++ {
		input = append(input, RunItem{
			Type:    RunItemMessage,
			Agent:   &Agent{Name: "assistant"},
			Message: &MessageOutput{Text: longText},
		})
	}

	runner := NewRunnerWithModel(model)
	_, err := runner.Run(context.Background(), &Agent{Name: "test"}, input, RunConfig{
		CompactionConfig: CompactionConfig{
			Enabled:                     true,
			TriggerTokens:               1000000,
			TargetTokens:                100,
			PreserveRecentItems:         1,
			PreserveInitialUserMessages: 1,
			SummaryBulletLimit:          2,
		},
		ModelSettings: ModelSettings{MaxTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	firstText := Items.ExtractText(model.requests[0].Input)
	if contains(firstText, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("first request input = %q, did not expect preflight compaction", firstText)
	}
	secondText := Items.ExtractText(model.requests[1].Input)
	if !contains(secondText, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("second request input = %q, want forced compacted history summary", secondText)
	}
}

func TestRunnerPrunesBeforeProviderCompactionItem(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{
						Type: RunItemCompaction,
						Compaction: &CompactionData{
							ID:               "cmp_1",
							EncryptedContent: "encrypted-state",
						},
					},
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "call1", Name: "echo", Input: json.RawMessage(`"hello"`),
					}},
				},
			},
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
		},
	}
	echoTool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			return "echoed: " + string(input), nil
		},
	}
	var compactionEvents int
	runner := NewRunnerWithModel(model)
	_, err := runner.Run(context.Background(), &Agent{Name: "test", Tools: []Tool{echoTool}}, []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "old context should be pruned"}},
	}, RunConfig{
		CompactionRecorder: func(_, _ int, summary string) {
			compactionEvents++
			if !strings.Contains(summary, "openai_responses_compaction") {
				t.Fatalf("summary = %q, want OpenAI compaction metadata", summary)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compactionEvents != 1 {
		t.Fatalf("compaction events = %d, want 1", compactionEvents)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	second := model.requests[1].Input
	if got := Items.ExtractText(second); strings.Contains(got, "old context should be pruned") {
		t.Fatalf("second request text = %q, did not expect old context", got)
	}
	if len(second) == 0 || second[0].Type != RunItemCompaction {
		t.Fatalf("second request first item = %+v, want compaction", second)
	}
	var retainedToolOutput bool
	for _, item := range second {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && strings.Contains(item.ToolOutput.Content, "echoed") {
			retainedToolOutput = true
			break
		}
	}
	if !retainedToolOutput {
		t.Fatalf("second request = %+v, want tool output retained", second)
	}
}

func TestApplyCompactionCarryForwardAppendsCurrentState(t *testing.T) {
	compacted := []RunItem{
		{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: compactionCarryForwardPrefix + "\nstale state"},
		},
		{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: "provider compacted context"},
		},
	}

	got, carryForward := applyCompactionCarryForward(context.Background(), compacted, nil, RunConfig{
		Phase:               "checking",
		WorkingStateContext: "## Durable Working State\nLatest assistant summary: implementation already landed",
		CompactionCarryForward: func(context.Context) string {
			return "Live AgentRun state: mode=deep phase=checking step=reviewing\n\n## Durable Working State\nLatest assistant summary: implementation already landed"
		},
	})

	if carryForward == "" {
		t.Fatal("expected carry-forward text")
	}
	if !strings.Contains(carryForward, "phase=checking") || !strings.Contains(carryForward, "implementation already landed") {
		t.Fatalf("carryForward = %q, want live phase and durable work state", carryForward)
	}
	gotText := Items.ExtractText(got)
	if strings.Contains(gotText, "stale state") {
		t.Fatalf("got text = %q, did not expect stale carry-forward packet", gotText)
	}
	if !strings.Contains(gotText, compactionCarryForwardPrefix) {
		t.Fatalf("got text = %q, want carry-forward prefix", gotText)
	}
	last := got[len(got)-1]
	if last.Agent != nil {
		t.Fatalf("last carry-forward item Agent = %+v, want user role", last.Agent)
	}
}

func TestApplyCompactionCarryForwardPreservesRecentProviderTail(t *testing.T) {
	compacted := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "provider compacted context"}},
	}
	previous := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "original task"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call_lint", Name: "Bash", Input: json.RawMessage(`{"command":"pnpm lint"}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "call_lint", Content: "pnpm lint passed"}},
		{Type: RunItemMessage, Agent: &Agent{Name: "assistant"}, Message: &MessageOutput{Text: "implementation is ready for checking"}},
	}

	got, _ := applyCompactionCarryForward(context.Background(), compacted, previous, RunConfig{
		CompactionConfig: CompactionConfig{
			Enabled:             true,
			PreserveRecentItems: 3,
		},
	})

	hasLintOutput := false
	for _, item := range got {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && strings.Contains(item.ToolOutput.Content, "pnpm lint passed") {
			hasLintOutput = true
			break
		}
	}
	gotText := Items.ExtractText(got)
	if !hasLintOutput || !strings.Contains(gotText, "implementation is ready for checking") {
		t.Fatalf("got = %+v, text = %q, want recent provider tail preserved", got, gotText)
	}
}

func TestApplyCompactionCarryForwardPreservesRecentToolPairs(t *testing.T) {
	previous := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "old context"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call_1", Name: "Bash", Input: json.RawMessage(`{"cmd":"pwd"}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "call_1", Content: "/repo"}},
	}
	compacted := []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "encrypted-state"}},
	}

	got, _ := applyCompactionCarryForward(context.Background(), compacted, previous, RunConfig{
		CompactionConfig: CompactionConfig{PreserveRecentItems: 1},
	})

	callIdx, outputIdx := toolPairPositions(got, "call_1")
	if callIdx < 0 || outputIdx < 0 {
		t.Fatalf("got items = %+v, want both tool call and output", got)
	}
	if callIdx > outputIdx {
		t.Fatalf("tool pair order = call %d output %d, want call before output", callIdx, outputIdx)
	}
}

func TestApplyCompactionCarryForwardRepairsProviderOrphanToolOutput(t *testing.T) {
	previous := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "old context"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "call_1", Name: "Bash", Input: json.RawMessage(`{"cmd":"pwd"}`)}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "call_1", Content: "/repo"}},
	}
	compacted := []RunItem{
		{Type: RunItemCompaction, Compaction: &CompactionData{ID: "cmp_1", EncryptedContent: "encrypted-state"}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "call_1", Content: "/repo"}},
	}

	got, _ := applyCompactionCarryForward(context.Background(), compacted, previous, RunConfig{
		CompactionConfig: CompactionConfig{PreserveRecentItems: 0},
	})

	callIdx, outputIdx := toolPairPositions(got, "call_1")
	if callIdx < 0 || outputIdx < 0 {
		t.Fatalf("got items = %+v, want repaired tool call and output", got)
	}
	if callIdx > outputIdx {
		t.Fatalf("tool pair order = call %d output %d, want call before output", callIdx, outputIdx)
	}
}

func TestRunnerHandoffHistoryCompactsForwardedInput(t *testing.T) {
	expertAgent := &Agent{Name: "expert", Instructions: "Expert agent"}

	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "h1", Name: "transfer_to_expert", Input: json.RawMessage(`{}`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "Expert response"}},
				},
			},
		},
	}

	longText := strings.Repeat("handoff history should be summarized to keep the forwarded context bounded. ", 12)
	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: longText}}}
	for i := 0; i < 8; i++ {
		input = append(input, RunItem{
			Type:    RunItemMessage,
			Agent:   &Agent{Name: "assistant"},
			Message: &MessageOutput{Text: longText},
		})
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name:     "router",
		Handoffs: []*Handoff{NewHandoff(expertAgent)},
	}

	_, err := runner.Run(context.Background(), agent, input, RunConfig{
		HandoffHistory: HandoffHistoryConfig{
			Enabled:             true,
			MaxTokens:           10,
			TargetTokens:        200,
			PreserveRecentItems: 1,
			SummaryBulletLimit:  2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	secondInput := Items.ExtractText(model.requests[1].Input)
	if !contains(secondInput, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("second request input = %q, want compacted handoff summary", secondInput)
	}
}

func TestRunnerReportsCompactionFailure(t *testing.T) {
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "ok"}}}},
		},
	}

	runner := NewRunnerWithModel(model)
	agent := &Agent{Name: "test"}
	input := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: strings.Repeat("important user objective ", 12)}},
	}

	reported := false
	_, err := runner.Run(context.Background(), agent, input, RunConfig{
		CompactionConfig: CompactionConfig{
			Enabled:                     true,
			TriggerTokens:               10,
			TargetTokens:                5,
			PreserveRecentItems:         1,
			PreserveInitialUserMessages: 1,
			SummaryBulletLimit:          1,
		},
		CompactionFailureReporter: func(scope, reason string, tokensBefore, tokensAfter int) {
			reported = scope == "run" && reason == "no-removable-history" && tokensBefore > 0 && tokensAfter >= 0
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reported {
		t.Fatal("expected compaction failure reporter to be invoked")
	}
}

func contains(haystack, needle string) bool {
	return haystack != "" && needle != "" && strings.Contains(haystack, needle)
}

func runItemsContainToolOutput(items []RunItem, needle string) bool {
	for _, item := range items {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && strings.Contains(item.ToolOutput.Content, needle) {
			return true
		}
	}
	return false
}

func runItemsContainText(items []RunItem, needle string) bool {
	for _, item := range items {
		if item.Message != nil && strings.Contains(item.Message.Text, needle) {
			return true
		}
		if item.ToolCall != nil && strings.Contains(string(item.ToolCall.Input), needle) {
			return true
		}
		if item.ToolOutput != nil && strings.Contains(item.ToolOutput.Content, needle) {
			return true
		}
		if item.Reasoning != nil && strings.Contains(item.Reasoning.Text, needle) {
			return true
		}
		if item.Compaction != nil && strings.Contains(item.Compaction.EncryptedContent, needle) {
			return true
		}
	}
	return false
}

func toolPairPositions(items []RunItem, callID string) (int, int) {
	callIdx, outputIdx := -1, -1
	for idx, item := range items {
		if item.Type == RunItemToolCall && item.ToolCall != nil && item.ToolCall.ID == callID && callIdx < 0 {
			callIdx = idx
		}
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID == callID && outputIdx < 0 {
			outputIdx = idx
		}
	}
	return callIdx, outputIdx
}

type captureTracingProcessor struct {
	spans []*Span
}

func (p *captureTracingProcessor) OnTraceStart(*Trace) {}
func (p *captureTracingProcessor) OnTraceEnd(*Trace)   {}
func (p *captureTracingProcessor) OnSpanStart(*Span)   {}
func (p *captureTracingProcessor) Flush()              {}
func (p *captureTracingProcessor) OnSpanEnd(s *Span) {
	cp := *s
	p.spans = append(p.spans, &cp)
}

func (p *captureTracingProcessor) functionOutputContains(needle string) bool {
	for _, span := range p.spans {
		switch data := span.Data.(type) {
		case FunctionSpanData:
			if strings.Contains(data.Output, needle) {
				return true
			}
		case *FunctionSpanData:
			if data != nil && strings.Contains(data.Output, needle) {
				return true
			}
		}
	}
	return false
}

// testRunHooks is a test implementation of RunHooks with configurable callbacks.
type testRunHooks struct {
	onAgentStart func(*RunContext, *Agent)
	onAgentEnd   func(*RunContext, *Agent, any)
	onHandoff    func(*RunContext, *Agent, *Agent)
	onToolStart  func(*RunContext, *Agent, Tool, ToolCallData)
	onToolEnd    func(*RunContext, *Agent, Tool, ToolCallData, ToolResult)
	onLLMStart   func(*RunContext, *Agent)
	onLLMEnd     func(*RunContext, *Agent, *ModelResponse)
}

func (h *testRunHooks) OnAgentStart(ctx *RunContext, a *Agent) {
	if h.onAgentStart != nil {
		h.onAgentStart(ctx, a)
	}
}
func (h *testRunHooks) OnAgentEnd(ctx *RunContext, a *Agent, o any) {
	if h.onAgentEnd != nil {
		h.onAgentEnd(ctx, a, o)
	}
}
func (h *testRunHooks) OnHandoff(ctx *RunContext, from *Agent, to *Agent) {
	if h.onHandoff != nil {
		h.onHandoff(ctx, from, to)
	}
}
func (h *testRunHooks) OnToolStart(ctx *RunContext, a *Agent, tool Tool, call ToolCallData) {
	if h.onToolStart != nil {
		h.onToolStart(ctx, a, tool, call)
	}
}
func (h *testRunHooks) OnToolEnd(ctx *RunContext, a *Agent, tool Tool, call ToolCallData, r ToolResult) {
	if h.onToolEnd != nil {
		h.onToolEnd(ctx, a, tool, call, r)
	}
}
func (h *testRunHooks) OnLLMStart(ctx *RunContext, a *Agent) {
	if h.onLLMStart != nil {
		h.onLLMStart(ctx, a)
	}
}
func (h *testRunHooks) OnLLMEnd(ctx *RunContext, a *Agent, r *ModelResponse) {
	if h.onLLMEnd != nil {
		h.onLLMEnd(ctx, a, r)
	}
}
