package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunnerActionAuthorizationDecisions(t *testing.T) {
	tests := []struct {
		name              string
		authorization     ActionAuthorization
		authorizerErr     error
		needsApproval     bool
		wantExecuted      bool
		wantInterrupted   bool
		wantOutputError   bool
		wantAuditDecision ActionDecision
	}{
		{name: "allow executes", authorization: ActionAuthorization{Decision: ActionDecisionAllow, Rule: "safe-write", Risk: ActionRiskLow}, wantExecuted: true, wantAuditDecision: ActionDecisionAllow},
		{name: "deny returns error", authorization: ActionAuthorization{Decision: ActionDecisionDeny, Reason: "outside trusted scope", Rule: "scope", Risk: ActionRiskHigh}, wantOutputError: true, wantAuditDecision: ActionDecisionDeny},
		{name: "ask interrupts", authorization: ActionAuthorization{Decision: ActionDecisionAsk, Reason: "human checkpoint", Rule: "checkpoint", Risk: ActionRiskMedium}, wantInterrupted: true, wantAuditDecision: ActionDecisionAsk},
		{name: "allow preserves tool approval", authorization: ActionAuthorization{Decision: ActionDecisionAllow}, needsApproval: true, wantInterrupted: true, wantAuditDecision: ActionDecisionAllow},
		{name: "authorizer error denies", authorization: ActionAuthorization{Decision: ActionDecisionAllow}, authorizerErr: errors.New("reviewer unavailable"), wantOutputError: true, wantAuditDecision: ActionDecisionDeny},
		{name: "classify terminal output denies", authorization: ActionAuthorization{Decision: ActionDecisionClassify}, wantOutputError: true, wantAuditDecision: ActionDecisionDeny},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executed := false
			result, err := NewRunnerWithModel(&mockModel{responses: []*ModelResponse{
				authorizationToolCallResponse("call-1", "write_file"),
			}}).Run(context.Background(), &Agent{
				Name:            "test",
				Tools:           []Tool{actionTestTool("write_file", &executed, test.needsApproval)},
				ToolUseBehavior: StopOnFirstTool,
			}, nil, RunConfig{
				WorkDir: "/workspace/repo",
				ActionAuthorizer: actionAuthorizerFunc(func(_ context.Context, request ActionRequest) (ActionAuthorization, error) {
					if request.ToolName != "write_file" || request.WorkDir != "/workspace/repo" || request.Provenance != "model-tool-call" {
						t.Fatalf("ActionRequest = %#v", request)
					}
					return test.authorization, test.authorizerErr
				}),
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if executed != test.wantExecuted {
				t.Fatalf("executed = %v, want %v", executed, test.wantExecuted)
			}
			if result.IsInterrupted() != test.wantInterrupted {
				t.Fatalf("IsInterrupted() = %v, want %v", result.IsInterrupted(), test.wantInterrupted)
			}
			if len(result.ActionAuditRecords) != 1 || result.ActionAuditRecords[0].Authorization.Decision != test.wantAuditDecision {
				t.Fatalf("ActionAuditRecords = %#v, want one %q record", result.ActionAuditRecords, test.wantAuditDecision)
			}
			if test.wantOutputError {
				output := firstToolOutput(t, result)
				if !output.IsError || !strings.Contains(output.Content, "Action denied:") || !strings.Contains(output.Content, "do not route around it") {
					t.Fatalf("ToolOutput = %#v, want denied model-visible error", output)
				}
			}
		})
	}
}
