package agent_runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestAgentRuntimeExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	agent := &agentsdk.Agent{
		Name:  "runtime",
		Model: model,
		InstructionsFn: func(_ *agentsdk.RunContext, agent *agentsdk.Agent) string {
			return "You MUST reply with exactly the single word READY in upper case. No punctuation. Agent name: " + agent.Name
		},
		ModelSettings: agentsdk.ModelSettings{MaxTokens: 64},
	}

	result, err := runner.Run(context.Background(), agent, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Confirm the runtime is ready."},
		},
	}, agentsdk.RunConfig{
		MaxTurns: 1,

		ModelSettings: agentsdk.ModelSettings{ReasoningEffort: "low"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(result.FinalText()), "READY") {
		t.Fatalf("FinalText() = %q, want sentinel READY", result.FinalText())
	}
	if result.LastAgent.Name != "runtime" || len(result.RawResponses) == 0 {
		t.Fatalf("unexpected run result: %+v", result)
	}
	if result.Usage.OutputTokens == 0 {
		t.Fatalf("expected non-zero output tokens, got %+v", result.Usage)
	}
}

type recordingHooks struct {
	agentsdk.NoOpRunHooks
	starts   int
	ends     int
	llmStart int
	llmEnd   int
	toolStrt int
	toolEnd  int
}

func (h *recordingHooks) OnAgentStart(_ *agentsdk.RunContext, _ *agentsdk.Agent) { h.starts++ }
func (h *recordingHooks) OnAgentEnd(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ any) {
	h.ends++
}
func (h *recordingHooks) OnLLMStart(_ *agentsdk.RunContext, _ *agentsdk.Agent) { h.llmStart++ }
func (h *recordingHooks) OnLLMEnd(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ *agentsdk.ModelResponse) {
	h.llmEnd++
}
func (h *recordingHooks) OnToolStart(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, _ agentsdk.ToolCallData) {
	h.toolStrt++
}
func (h *recordingHooks) OnToolEnd(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, _ agentsdk.ToolCallData, _ agentsdk.ToolResult) {
	h.toolEnd++
}

func TestAgentRuntimeMultiTurnToolLoop(t *testing.T) {
	runner, model := liverunner.Runner(t)

	var calls atomic.Int32
	pingTool := &agentsdk.FunctionTool{
		ToolName:        "ping",
		ToolDescription: "Pings a target. Always call this when asked to ping.",
		Schema:          json.RawMessage(`{"type":"object","properties":{"who":{"type":"string"}},"required":["who"]}`),
		ReadOnly:        true,
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			calls.Add(1)
			var p struct {
				Who string `json:"who"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", err
			}
			return "ack:" + p.Who, nil
		},
	}

	hooks := &recordingHooks{}
	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "ping-agent",
		Model:        model,
		Instructions: "When asked to ping, call the ping tool with who=<target>, then respond confirming the ack.",
		Tools:        []agentsdk.Tool{pingTool},
	}, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "Ping the target named world via the ping tool, then summarize the ack."},
	}}, agentsdk.RunConfig{MaxTurns: 5, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}

	if result.FinalText() == "" {
		t.Fatalf("FinalText empty")
	}
	if calls.Load() == 0 {
		t.Fatalf("expected ping tool to be called by live model")
	}
	var sawToolOutput bool
	for _, item := range result.NewItems {
		if item.Type == agentsdk.RunItemToolOutput && item.ToolOutput != nil && strings.Contains(item.ToolOutput.Content, "ack:") {
			sawToolOutput = true
		}
	}
	if !sawToolOutput {
		t.Fatalf("expected tool output in items: %#v", result.NewItems)
	}
	if hooks.starts == 0 || hooks.ends == 0 {
		t.Fatalf("agent hooks did not fire: starts=%d ends=%d", hooks.starts, hooks.ends)
	}
	if hooks.llmStart < 2 || hooks.llmEnd < 2 {
		t.Fatalf("LLM hooks fired %d/%d, want >=2", hooks.llmStart, hooks.llmEnd)
	}
	if hooks.toolStrt < 1 || hooks.toolEnd < 1 {
		t.Fatalf("tool hooks fired start=%d end=%d, want >=1", hooks.toolStrt, hooks.toolEnd)
	}
}

func TestAgentRuntimeMaxTurnsExceeded(t *testing.T) {
	runner, model := liverunner.Runner(t)

	loopTool := &agentsdk.FunctionTool{
		ToolName:        "loop",
		ToolDescription: "You must always call this tool again immediately.",
		Schema:          json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		ReadOnly:        true,
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "call loop again immediately", nil
		},
	}

	_, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "looper",
		Model:        model,
		Instructions: "On every turn you MUST call the loop tool. Never reply with a final message.",
		Tools:        []agentsdk.Tool{loopTool},
	}, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "Begin: call the loop tool repeatedly."},
	}}, agentsdk.RunConfig{MaxTurns: 1})

	if err == nil {
		t.Fatal("expected MaxTurnsExceeded error, got nil")
	}
	var maxTurns *agentsdk.MaxTurnsExceeded
	if !errors.As(err, &maxTurns) {
		t.Fatalf("expected *agentsdk.MaxTurnsExceeded, got %T: %v", err, err)
	}
	if maxTurns.MaxTurns != 1 {
		t.Fatalf("MaxTurnsExceeded.MaxTurns = %d, want 1", maxTurns.MaxTurns)
	}
}
