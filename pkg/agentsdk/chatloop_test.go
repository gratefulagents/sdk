package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
)

type scriptedChatModel struct {
	responses []*ModelResponse
	requests  []ModelRequest
}

func (m *scriptedChatModel) GetResponse(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.requests = append(m.requests, req)
	idx := len(m.requests) - 1
	if idx >= len(m.responses) {
		return nil, errors.New("unexpected model call")
	}
	resp := *m.responses[idx]
	resp.Items = append([]RunItem(nil), m.responses[idx].Items...)
	return &resp, nil
}

func (m *scriptedChatModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
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
			events <- ModelStreamEvent{Type: ModelStreamItemDone, Item: &item}
		}
		events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
		done <- resp
	}()
	return NewModelStream(events, done), nil
}

func (m *scriptedChatModel) GetRetryAdvice(error) *ModelRetryAdvice {
	return &ModelRetryAdvice{ShouldRetry: false}
}

func (m *scriptedChatModel) CalculateCost(Usage) float64 { return 0 }
func (m *scriptedChatModel) Provider() string            { return "scripted" }

type approvingGate struct {
	approved bool
}

func (g approvingGate) ApproveTool(context.Context, ToolApprovalRequest) (bool, string, error) {
	if g.approved {
		return true, "", nil
	}
	return false, "denied", nil
}

func TestChatLoopResolvesToolApprovalAndResumes(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
			ID:    "mutate_call",
			Name:  "mutate",
			Input: json.RawMessage(`{"value":"x"}`),
		}}}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "approved done"}}}},
	}}
	executed := false
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "mutates",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "mutated", nil
		},
	}

	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{tool}},
		RunConfig:    RunConfig{MaxTurns: 3, ToolPolicy: &ToolPolicy{ApprovalRequired: true}},
		ApprovalGate: approvingGate{approved: true},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatal("expected approved tool to execute")
	}
	if result.FinalText() != "approved done" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
}

func TestChatLoopApprovedToolUsesRunnerGuardrails(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{
		{
			Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "mutate_call",
				Name:  "mutate",
				Input: json.RawMessage(`{"value":"secret"}`),
			}}},
		},
		{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "guardrail handled"}}},
		},
	}}
	executed := false
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "mutates",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "mutated", nil
		},
	}

	result, err := NewChatLoop(ChatLoopOptions{
		Runner: NewRunnerWithModel(model),
		Agent:  &Agent{Name: "loop", Model: "demo", Tools: []Tool{tool}},
		RunConfig: RunConfig{
			MaxTurns:   2,
			ToolPolicy: &ToolPolicy{ApprovalRequired: true},
			ToolInputGuardrails: []ToolInputGuardrail{{
				Name: "block-secret",
				Fn: func(_ *RunContext, _ *Agent, _ Tool, input json.RawMessage) (*GuardrailResult, error) {
					if string(input) == `{"value":"secret"}` {
						return &GuardrailResult{TripwireTriggered: true, Output: "blocked"}, nil
					}
					return &GuardrailResult{}, nil
				},
			}},
		},
		ApprovalGate: approvingGate{approved: true},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatal("approved tool executed despite tool input guardrail")
	}
	if result.FinalText() != "guardrail handled" {
		t.Fatalf("FinalText() = %q, want guardrail handled", result.FinalText())
	}
	if result == nil || len(result.NewItems) < 3 || !containsToolOutput(result.NewItems, "tool input guardrail") {
		t.Fatalf("result.NewItems = %#v, want approval plus guarded tool output and final", result)
	}
}

func TestChatLoopApprovedToolUsesRunnerTimeoutPolicy(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
			ID:    "slow_call",
			Name:  "slow_mutate",
			Input: json.RawMessage(`{"value":"x"}`),
		}}}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "resumed after timeout"}}}},
	}}
	tool := &FunctionTool{
		ToolName:        "slow_mutate",
		ToolDescription: "blocks until its context is canceled",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	start := time.Now()
	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{tool}},
		RunConfig:    RunConfig{MaxTurns: 3, ToolPolicy: &ToolPolicy{ApprovalRequired: true, DefaultTimeout: 1}},
		ApprovalGate: approvingGate{approved: true},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("approved tool took %s, want runner timeout policy to interrupt it", elapsed)
	}
	if result.FinalText() != "resumed after timeout" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
	if !containsToolOutput(result.NewItems, "timed out") {
		t.Fatalf("NewItems = %#v, want timeout tool output from approved execution", result.NewItems)
	}
}

func TestChatLoopApprovedToolUsesRunnerOutputGuardrails(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{
		{
			Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "mutate_call",
				Name:  "mutate",
				Input: json.RawMessage(`{"value":"x"}`),
			}}},
		},
		{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "guardrail handled"}}},
		},
	}}
	executed := false
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "mutates",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "secret output", nil
		},
	}

	result, err := NewChatLoop(ChatLoopOptions{
		Runner:       NewRunnerWithModel(model),
		Agent:        &Agent{Name: "loop", Model: "demo", Tools: []Tool{tool}},
		RunConfig:    RunConfig{MaxTurns: 2, ToolPolicy: &ToolPolicy{ApprovalRequired: true}, ToolOutputGuardrails: []ToolOutputGuardrail{blockToolOutputGuardrail("secret")}},
		ApprovalGate: approvingGate{approved: true},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatal("tool should execute before output guardrail checks its result")
	}
	if result.FinalText() != "guardrail handled" {
		t.Fatalf("FinalText() = %q, want guardrail handled", result.FinalText())
	}
	if result == nil || len(result.NewItems) < 3 || !containsToolOutput(result.NewItems, "tool output guardrail") {
		t.Fatalf("result.NewItems = %#v, want approval plus guarded tool output and final", result)
	}
}

func TestChatLoopLoadsConfigGuardrailRules(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{
		{
			Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "mutate_call",
				Name:  "mutate",
				Input: json.RawMessage(`{"value":"secret"}`),
			}}},
		},
		{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "guardrail handled"}}},
		},
	}}
	executed := false
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "mutates",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "mutated", nil
		},
	}

	result, err := NewChatLoop(ChatLoopOptions{
		Runner: NewRunnerWithModel(model),
		Agent:  &Agent{Name: "loop", Model: "demo", Tools: []Tool{tool}},
		RunConfig: RunConfig{
			MaxTurns: 2,
		},
		ConfigSource: staticConfigSource{
			mode: PermissionModeDangerFullAccess,
			rules: []GuardrailRule{{
				Name:        "block-secret",
				Type:        "tool-input",
				Regex:       "secret",
				ToolPattern: "mutate",
				Action:      "block",
				Message:     "blocked by config rule",
			}},
		},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatal("tool executed despite config guardrail rule")
	}
	if result.FinalText() != "guardrail handled" || !containsToolOutput(result.NewItems, "tool input guardrail") {
		t.Fatalf("result = %#v, want guarded tool output and final", result)
	}
}

func TestChatLoopLoadsConfigToolOutputGuardrailRules(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{
		{
			Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
				ID:    "mutate_call",
				Name:  "mutate",
				Input: json.RawMessage(`{"value":"x"}`),
			}}},
		},
		{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "guardrail handled"}}},
		},
	}}
	executed := false
	tool := &FunctionTool{
		ToolName:        "mutate",
		ToolDescription: "mutates",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "secret output", nil
		},
	}

	result, err := NewChatLoop(ChatLoopOptions{
		Runner:    NewRunnerWithModel(model),
		Agent:     &Agent{Name: "loop", Model: "demo", Tools: []Tool{tool}},
		RunConfig: RunConfig{MaxTurns: 2},
		ConfigSource: staticConfigSource{
			mode: PermissionModeDangerFullAccess,
			rules: []GuardrailRule{{
				Name:        "block-secret-output",
				Type:        "tool-output",
				Regex:       "secret",
				ToolPattern: "mutate",
				Action:      "block",
				Message:     "blocked output by config rule",
			}},
		},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatal("tool should execute before config output guardrail checks its result")
	}
	if result.FinalText() != "guardrail handled" || !containsToolOutput(result.NewItems, "tool output guardrail") {
		t.Fatalf("result = %#v, want guarded tool output and final", result)
	}
}

func TestChatLoopInvalidConfigGuardrailRulesFailClosedBeforeModel(t *testing.T) {
	model := &scriptedChatModel{responses: []*ModelResponse{{
		Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "should not run"}}},
	}}}

	_, err := NewChatLoop(ChatLoopOptions{
		Runner:    NewRunnerWithModel(model),
		Agent:     &Agent{Name: "loop", Model: "demo"},
		RunConfig: RunConfig{MaxTurns: 1},
		ConfigSource: staticConfigSource{
			mode: PermissionModeDangerFullAccess,
			rules: []GuardrailRule{{
				Name:   "bad-regex",
				Type:   "tool-input",
				Regex:  "[",
				Action: "block",
			}},
		},
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "compile guardrail rules") {
		t.Fatalf("err = %v, want compile guardrail rules failure", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests = %d, want 0 after invalid config guardrail rule", len(model.requests))
	}
}

func blockToolOutputGuardrail(needle string) ToolOutputGuardrail {
	return ToolOutputGuardrail{
		Name: "block-output",
		Fn: func(_ *RunContext, _ *Agent, _ Tool, result ToolResult) (*GuardrailResult, error) {
			return &GuardrailResult{TripwireTriggered: strings.Contains(result.Content, needle)}, nil
		},
	}
}

func containsToolOutput(items []RunItem, needle string) bool {
	for _, item := range items {
		if item.ToolOutput != nil && strings.Contains(item.ToolOutput.Content, needle) {
			return true
		}
	}
	return false
}

type staticConfigSource struct {
	mode  PermissionMode
	rules []GuardrailRule
}

func (s staticConfigSource) PermissionMode(context.Context) (PermissionMode, error) {
	if s.mode == "" {
		return PermissionModeWorkspaceWrite, nil
	}
	return s.mode, nil
}

func (staticConfigSource) ModeSnapshot(context.Context) (*sdkmode.TemplateSpec, error) {
	return nil, nil
}

func (s staticConfigSource) GuardrailRules(context.Context) ([]GuardrailRule, error) {
	return append([]GuardrailRule(nil), s.rules...), nil
}

func (staticConfigSource) RoleCatalog(context.Context) (RoleCatalog, error) {
	return nil, nil
}

func (staticConfigSource) MCPServers(context.Context) (map[string]MCPServerConfig, error) {
	return nil, nil
}

func (staticConfigSource) ModeDirective(context.Context) (string, error) {
	return "", nil
}

func (staticConfigSource) HandoffHistory(context.Context) ([]RunItem, error) {
	return nil, nil
}
