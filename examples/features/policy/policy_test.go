package policy_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkpolicy "github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

func TestPolicyExample(t *testing.T) {
	defaultPolicy := sdkpolicy.RuntimePolicy{}.Normalize()
	if defaultPolicy.PermissionMode != sdkpolicy.PermissionModeWorkspaceWrite || !defaultPolicy.EnableMCP {
		t.Fatalf("default policy = %+v", defaultPolicy)
	}

	readOnlyRuntime := sdkpolicy.RuntimePolicy{PermissionMode: sdkpolicy.PermissionModeReadOnly}
	toolPolicy := sdkpolicy.NewToolPolicy(readOnlyRuntime)
	if toolPolicy.AllowsWriteTools() {
		t.Fatal("read-only tool policy should not allow write tools")
	}
	if sdkpolicy.PermissionModeReadOnly.AllowsMCPTool(false) {
		t.Fatal("read-only mode should reject MCP tools without a read-only hint")
	}
	if !sdkpolicy.PermissionModeDangerFullAccess.AllowsMCPTool(false) {
		t.Fatal("danger-full-access mode should allow MCP tools")
	}
}

func TestRunnerToolPolicyExample(t *testing.T) {
	runner, model := liverunner.Runner(t)
	agent := &agentsdk.Agent{
		Name:         "policy-runner",
		Model:        model,
		Instructions: "You must call the mutate tool with path=file.txt to satisfy any write request. Do not respond without calling it.",
		Tools: []agentsdk.Tool{
			&agentsdk.FunctionTool{
				ToolName:        "mutate",
				ToolDescription: "writes a file. Always call this when asked to write.",
				Schema:          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
				Fn: func(context.Context, json.RawMessage) (string, error) {
					return "wrote", nil
				},
			},
		},
	}

	result, err := runner.Run(context.Background(), agent, []agentsdk.RunItem{
		{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Please write file.txt by calling the mutate tool."}},
	}, agentsdk.RunConfig{
		MaxTurns: 2,

		ToolPolicy: &agentsdk.ToolPolicy{ApprovalRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Interruption == nil || result.Interruption.ToolName != "mutate" {
		t.Fatalf("interruption = %#v, want mutate approval", result.Interruption)
	}
}
