package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRunnerHeldMutationBlocksLaterMutationsInBatch(t *testing.T) {
	tests := []struct {
		name       string
		approval   bool
		authorizer ActionAuthorizer
	}{
		{name: "needs approval", approval: true},
		{name: "authorizer ask", authorizer: actionAuthorizerFunc(func(_ context.Context, req ActionRequest) (ActionAuthorization, error) {
			if req.ToolName == "first_write" {
				return ActionAuthorization{Decision: ActionDecisionAsk}, nil
			}
			return ActionAuthorization{Decision: ActionDecisionAllow}, nil
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var firstRan, secondRan, readRan bool
			first := actionTestTool("first_write", &firstRan, test.approval)
			second := actionTestTool("second_write", &secondRan, false)
			read := actionTestTool("read_file", &readRan, false)
			read.ReadOnly = true
			model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "first_write", Input: json.RawMessage(`{}`)}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "second_write", Input: json.RawMessage(`{}`)}},
				{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c3", Name: "read_file", Input: json.RawMessage(`{}`)}},
			}}}}
			result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{
				Name:  "test",
				Tools: []Tool{first, second, read},
			}, nil, RunConfig{ActionAuthorizer: test.authorizer})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if firstRan || secondRan {
				t.Fatalf("first ran = %v, second ran = %v; no mutation may run ahead of the held call", firstRan, secondRan)
			}
			if !readRan {
				t.Fatal("read-only call should still execute")
			}
			if len(result.Interruptions) != 1 || result.Interruptions[0].ToolCallID != "c1" {
				t.Fatalf("Interruptions = %#v, want only c1", result.Interruptions)
			}
			var deferred *ToolOutputData
			for _, item := range result.NewItems {
				if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID == "c2" {
					deferred = item.ToolOutput
				}
			}
			if deferred == nil || !deferred.IsError || !strings.Contains(deferred.Content, "pending approval") {
				t.Fatalf("c2 output = %#v, want a paired not-executed error", deferred)
			}
		})
	}
}

func TestRunnerRecoversPanicAndKeepsPartialResult(t *testing.T) {
	calls := 0
	var ran bool
	model := &mockModel{responses: []*ModelResponse{authorizationToolCallResponse("c1", "write_file")}}
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{
		Name:  "test",
		Tools: []Tool{actionTestTool("write_file", &ran, false)},
		InstructionsFn: func(*RunContext, *Agent) string {
			calls++
			if calls > 1 {
				panic("instructions exploded")
			}
			return "be helpful"
		},
	}, nil, RunConfig{MaxTurns: 5})
	if err == nil || !strings.Contains(err.Error(), "instructions exploded") {
		t.Fatalf("err = %v, want recovered panic", err)
	}
	if !ran {
		t.Fatal("first turn's tool should have run")
	}
	if result == nil || firstToolOutput(t, result).CallID != "c1" {
		t.Fatalf("result = %#v, want partial result with the first turn's items", result)
	}
}

func TestRunnerRecoversPanicBeforeAnyState(t *testing.T) {
	result, err := NewRunnerWithModel(&mockModel{}).Run(context.Background(), &Agent{
		Name:           "test",
		InstructionsFn: func(*RunContext, *Agent) string { panic("boom") },
	}, nil, RunConfig{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want recovered panic", err)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil before any state accumulated", result)
	}
}

func maliciousCarryForwardGuard(called *int) InputGuardrail {
	return InputGuardrail{
		Name: "scan",
		Fn: func(_ *RunContext, _ *Agent, items []RunItem) (*GuardrailResult, error) {
			*called++
			for _, it := range items {
				if it.Type == RunItemMessage && it.Message != nil && strings.Contains(it.Message.Text, "MALICIOUS") {
					return &GuardrailResult{TripwireTriggered: true}, nil
				}
			}
			return &GuardrailResult{}, nil
		},
	}
}

func maliciousCarryForward(context.Context) string { return "MALICIOUS attacker-controlled state" }

func TestRunnerHandoffCarryForwardIsRunThroughTargetInputGuardrails(t *testing.T) {
	called := 0
	expert := &Agent{Name: "expert", InputGuardrails: []InputGuardrail{maliciousCarryForwardGuard(&called)}}
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "h1", Name: "transfer_to_expert", Input: json.RawMessage(`{}`)}}}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "expert response"}}}},
	}}
	longText := strings.Repeat("handoff history should be summarized before it is forwarded. ", 12)
	var input []RunItem
	for i := 0; i < 8; i++ {
		input = append(input, RunItem{Type: RunItemMessage, Message: &MessageOutput{Text: longText}})
	}
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{
		Name:     "router",
		Handoffs: []*Handoff{NewHandoff(expert)},
	}, input, RunConfig{
		HandoffHistory:         HandoffHistoryConfig{Enabled: true, MaxTokens: 10, TargetTokens: 200, PreserveRecentItems: 1, SummaryBulletLimit: 2},
		CompactionCarryForward: maliciousCarryForward,
	})
	var trip *InputGuardrailTripwireTriggered
	if !errors.As(err, &trip) {
		t.Fatalf("err = %v (guard called %d times), want target agent's input guardrail tripwire", err, called)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1: the target must not see the carry-forward", len(model.requests))
	}
}

func TestRunnerResponseCompactionCarryForwardIsRunThroughInputGuardrails(t *testing.T) {
	called := 0
	var ran bool
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{
			{Type: RunItemCompaction, Compaction: &CompactionData{EncryptedContent: "opaque-blob"}},
			{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "write_file", Input: json.RawMessage(`{}`)}},
		}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
	}}
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{
		Name:            "test",
		Tools:           []Tool{actionTestTool("write_file", &ran, false)},
		InputGuardrails: []InputGuardrail{maliciousCarryForwardGuard(&called)},
	}, []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "hello"}}}, RunConfig{
		CompactionCarryForward: maliciousCarryForward,
	})
	var trip *InputGuardrailTripwireTriggered
	if !errors.As(err, &trip) {
		t.Fatalf("err = %v (guard called %d times), want input guardrail tripwire on response-compaction carry-forward", err, called)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1: the carry-forward must not reach the model", len(model.requests))
	}
}

func TestRunnerFallbackDoesNotConsumeRetryBudget(t *testing.T) {
	primary := &fallbackTestModel{
		provider:     "openai",
		errors:       []error{errors.New("subscription limit reached")},
		retryAdvices: []ModelRetryAdvice{{ShouldRetry: true, Reason: "429"}},
	}
	// The fallback's first call fails with an error only RetryPolicy retries.
	fallback := &fallbackTestModel{
		provider: "anthropic",
		errors:   []error{errors.New("transient hiccup")},
		responses: []*ModelResponse{nil, {
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done on fallback"}}},
		}},
		retryAdvices: []ModelRetryAdvice{{}},
	}
	provider := &fallbackTestProvider{models: map[string]*fallbackTestModel{
		"openai/gpt-primary":          primary,
		"anthropic/claude-sonnet-4-6": fallback,
	}}
	result, err := NewRunnerWithProvider(provider).Run(context.Background(), &Agent{
		Name:           "agent",
		Model:          "openai/gpt-primary",
		FallbackModels: []string{"anthropic/claude-sonnet-4-6"},
	}, nil, RunConfig{
		MaxTurns:    1,
		RetryPolicy: &RetryPolicy{MaxRetries: 1, Backoff: RetryBackoffSettings{InitialDelayMS: 1, MaxDelayMS: 1, Multiplier: 1}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v; the fallback switch must not use up the one-retry budget or the single turn", err)
	}
	if result.FinalText() != "done on fallback" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
	if len(fallback.requests) != 2 {
		t.Fatalf("fallback requests = %d, want 2 (initial + one policy retry)", len(fallback.requests))
	}
}
