package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	agentsdktools "github.com/gratefulagents/sdk/pkg/agentsdk/tools"
)

func TestFunctionToolExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	var calls atomic.Int32
	echoTool := &agentsdk.FunctionTool{
		ToolName:        "echo",
		ToolDescription: "Echoes the provided text back with an 'echo: ' prefix. Always call this tool when the user asks you to echo something.",
		Schema:          json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		ReadOnly:        true,
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			calls.Add(1)
			var params struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", err
			}
			return "echo: " + params.Text, nil
		},
	}

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "tool-user",
		Model:        model,
		Instructions: "You must call the echo tool to satisfy any echo request.",
		Tools:        []agentsdk.Tool{echoTool},
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Use the echo tool to echo the word hello, then summarize the result."},
		},
	}, agentsdk.RunConfig{MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatalf("expected echo tool to be called by live model; items=%#v", result.NewItems)
	}
	if result.FinalText() == "" {
		t.Fatal("expected non-empty final text after tool round trip")
	}

	var sawToolOutput bool
	for _, item := range result.NewItems {
		if item.Type == agentsdk.RunItemToolOutput && strings.Contains(item.ToolOutput.Content, "echo: ") {
			sawToolOutput = true
		}
	}
	if !sawToolOutput {
		t.Fatalf("tool output missing from result items: %#v", result.NewItems)
	}
}

func TestToolApprovalAndPolicyWrapperExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	writeTool := &agentsdk.FunctionTool{
		ToolName:        "write_file",
		ToolDescription: "Writes a file. Always call this tool when asked to write a file.",
		Schema:          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "wrote file", nil
		},
	}
	guardedTool := agentsdk.WrapWithPolicy(writeTool, true, 5)

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "approval-demo",
		Model:        model,
		Instructions: "You must call write_file with path=notes.txt to satisfy any write request.",
		Tools:        []agentsdk.Tool{guardedTool},
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Please write the file notes.txt by calling the write_file tool."},
		},
	}, agentsdk.RunConfig{MaxTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsInterrupted() {
		t.Fatalf("expected approval interruption; final=%q items=%#v", result.FinalText(), result.NewItems)
	}
	if result.Interruption.ToolName != "write_file" || guardedTool.TimeoutSeconds() != 5 {
		t.Fatalf("interruption = %+v timeout=%d", result.Interruption, guardedTool.TimeoutSeconds())
	}
}

// TestReadOnlyToolsRegistryFiltersMutatingToolsExample shows that tools
// constructed via the public registry with WithReadOnlyTools refuse to expose
// write/edit tools, while still surfacing read-only tools like Read and Glob.
// This is the security guarantee the policy and read-only modes rely on.
func TestReadOnlyToolsRegistryFiltersMutatingToolsExample(t *testing.T) {
	roReg := agentsdktools.NewRegistry(t.TempDir(), agentsdktools.WithReadOnlyTools())
	rwReg := agentsdktools.NewRegistry(t.TempDir())

	mustReadOnly := []string{"read_file", "glob", "grep"}
	mustNotExposeWrites := []string{"Write", "Edit"}

	for _, name := range mustReadOnly {
		if roReg.Get(name) == nil {
			t.Fatalf("read-only registry missing %q", name)
		}
	}
	for _, name := range mustNotExposeWrites {
		if roReg.Get(name) != nil {
			t.Fatalf("read-only registry exposed mutating tool %q", name)
		}
		if rwReg.Get(name) == nil {
			t.Fatalf("read-write registry missing %q (sanity check)", name)
		}
	}
}
