package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type actionAuthorizerFunc func(context.Context, ActionRequest) (ActionAuthorization, error)

func (f actionAuthorizerFunc) Authorize(ctx context.Context, request ActionRequest) (ActionAuthorization, error) {
	return f(ctx, request)
}

func authorizationToolCallResponse(callID, toolName string) *ModelResponse {
	return &ModelResponse{Items: []RunItem{{
		Type: RunItemToolCall,
		ToolCall: &ToolCallData{
			ID:    callID,
			Name:  toolName,
			Input: json.RawMessage(`{"target":"fixture"}`),
		},
	}}}
}

func actionTestTool(name string, executed *bool, needsApproval bool) *FunctionTool {
	return &FunctionTool{
		ToolName: name,
		Schema:   json.RawMessage(`{"type":"object","properties":{}}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			*executed = true
			return "executed", nil
		},
		Approval: needsApproval,
	}
}

func firstToolOutput(t *testing.T, result *RunResult) *ToolOutputData {
	t.Helper()
	for _, item := range result.NewItems {
		if item.Type == RunItemToolOutput && item.ToolOutput != nil {
			return item.ToolOutput
		}
	}
	t.Fatalf("NewItems = %#v, want tool output", result.NewItems)
	return nil
}

func TestRunnerExecutesToolsWhenActionAuthorizerIsNil(t *testing.T) {
	executed := false
	result, err := NewRunnerWithModel(&mockModel{responses: []*ModelResponse{
		authorizationToolCallResponse("call-1", "write_file"),
	}}).Run(context.Background(), &Agent{
		Name:            "test",
		Tools:           []Tool{actionTestTool("write_file", &executed, false)},
		ToolUseBehavior: StopOnFirstTool,
	}, nil, RunConfig{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !executed {
		t.Fatal("tool did not execute without an action authorizer")
	}
	if len(result.ActionAuditRecords) != 0 {
		t.Fatalf("ActionAuditRecords = %#v, want none", result.ActionAuditRecords)
	}
}

func TestNormalizeActionAuthorizationFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		auth ActionAuthorization
		err  error
		rule string
	}{
		{name: "error", auth: ActionAuthorization{Decision: ActionDecisionAllow}, err: errors.New("unavailable"), rule: actionAuthorizationErrorRule},
		{name: "empty", auth: ActionAuthorization{}, rule: actionAuthorizationInvalidRule},
		{name: "unknown", auth: ActionAuthorization{Decision: ActionDecision("unexpected")}, rule: actionAuthorizationInvalidRule},
		{name: "classify", auth: ActionAuthorization{Decision: ActionDecisionClassify}, rule: actionAuthorizationInvalidRule},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := NormalizeActionAuthorization(test.auth, test.err)
			if got.Decision != ActionDecisionDeny || got.Rule != test.rule || got.Reason == "" {
				t.Fatalf("NormalizeActionAuthorization() = %#v, want fail-closed deny rule %q", got, test.rule)
			}
		})
	}
}
