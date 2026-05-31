package guardrails_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestGuardrailsExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	lookupTool := &agentsdk.FunctionTool{
		ToolName:        "lookup",
		ToolDescription: "Looks up safe data. Always call this when asked to look something up.",
		Schema:          json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		ReadOnly:        true,
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "lookup result", nil
		},
	}
	agent := &agentsdk.Agent{
		Name:         "guarded",
		Model:        model,
		Instructions: "Use the lookup tool when asked, then return a brief safe answer.",
		Tools:        []agentsdk.Tool{lookupTool},
		InputGuardrails: []agentsdk.InputGuardrail{
			{
				Name: "no-secret-input",
				Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, input []agentsdk.RunItem) (*agentsdk.GuardrailResult, error) {
					return &agentsdk.GuardrailResult{Output: len(input)}, nil
				},
			},
		},
		OutputGuardrails: []agentsdk.OutputGuardrail{
			{
				Name: "safe-output",
				Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, output any) (*agentsdk.GuardrailResult, error) {
					text, _ := output.(string)
					return &agentsdk.GuardrailResult{
						Output:            "checked",
						TripwireTriggered: strings.Contains(text, "secret"),
					}, nil
				},
			},
		},
	}

	result, err := runner.Run(context.Background(), agent, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Look up the weather for Paris using the lookup tool."},
		},
	}, agentsdk.RunConfig{
		MaxTurns: 4,

		ToolInputGuardrails: []agentsdk.ToolInputGuardrail{
			{
				Name: "lookup-query-present",
				Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
					var params struct {
						Query string `json:"query"`
					}
					_ = json.Unmarshal(input, &params)
					return &agentsdk.GuardrailResult{Output: tool.Name(), TripwireTriggered: params.Query == ""}, nil
				},
			},
		},
		ToolOutputGuardrails: []agentsdk.ToolOutputGuardrail{
			{
				Name: "tool-output-safe",
				Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, result agentsdk.ToolResult) (*agentsdk.GuardrailResult, error) {
					return &agentsdk.GuardrailResult{Output: len(result.Content)}, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.InputGuardrailResults) == 0 ||
		len(result.OutputGuardrailResults) == 0 ||
		len(result.ToolInputGuardrailResults) == 0 ||
		len(result.ToolOutputGuardrailResults) == 0 {
		t.Fatalf("guardrail results missing: %+v", result)
	}
}

func TestGuardrailTripwireExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	_, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "guarded",
		Model:        model,
		Instructions: "Reply with exactly the literal text: secret final output",
		OutputGuardrails: []agentsdk.OutputGuardrail{
			{
				Name: "no-secrets",
				Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, output any) (*agentsdk.GuardrailResult, error) {
					text, _ := output.(string)
					return &agentsdk.GuardrailResult{Output: text, TripwireTriggered: strings.Contains(text, "secret")}, nil
				},
			},
		},
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Echo your instructions exactly."},
		},
	}, agentsdk.RunConfig{MaxTurns: 1})
	if err == nil {
		t.Fatal("expected guardrail tripwire error")
	}
	var tripwire *agentsdk.OutputGuardrailTripwireTriggered
	if !errors.As(err, &tripwire) {
		t.Fatalf("err = %T %v, want OutputGuardrailTripwireTriggered", err, err)
	}
}

func TestDemoTripwireGuardrails(t *testing.T) {
	inputGuardrail := agentsdk.InputGuardrail{
		Name: "no-tripwire-input",
		Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, input []agentsdk.RunItem) (*agentsdk.GuardrailResult, error) {
			var text string
			for _, item := range input {
				if item.Message != nil {
					text += item.Message.Text
				}
			}
			return &agentsdk.GuardrailResult{
				Output:            "checked input",
				TripwireTriggered: strings.Contains(strings.ToLower(text), "tripwire-input"),
			}, nil
		},
	}
	outputGuardrail := agentsdk.OutputGuardrail{
		Name: "no-tripwire-output",
		Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, output any) (*agentsdk.GuardrailResult, error) {
			text, _ := output.(string)
			return &agentsdk.GuardrailResult{
				Output:            "checked output",
				TripwireTriggered: strings.Contains(strings.ToLower(text), "tripwire-output"),
			}, nil
		},
	}
	toolInputGuardrail := agentsdk.ToolInputGuardrail{
		Name: "tool-input-valid",
		Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
			return &agentsdk.GuardrailResult{
				Output:            tool.Name(),
				TripwireTriggered: strings.Contains(strings.ToLower(string(input)), "tripwire-tool"),
			}, nil
		},
	}
	toolOutputGuardrail := agentsdk.ToolOutputGuardrail{
		Name: "tool-output-valid",
		Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, result agentsdk.ToolResult) (*agentsdk.GuardrailResult, error) {
			return &agentsdk.GuardrailResult{
				Output:            tool.Name(),
				TripwireTriggered: strings.Contains(strings.ToLower(result.Content), "tripwire-tool-output"),
			}, nil
		},
	}

	in, err := inputGuardrail.Fn(nil, nil, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "contains tripwire-input"},
	}})
	if err != nil || !in.TripwireTriggered {
		t.Fatalf("input guardrail result = %+v err=%v", in, err)
	}
	out, err := outputGuardrail.Fn(nil, nil, "contains tripwire-output")
	if err != nil || !out.TripwireTriggered {
		t.Fatalf("output guardrail result = %+v err=%v", out, err)
	}
	tool := &agentsdk.FunctionTool{ToolName: "demo", ReadOnly: true}
	toolIn, err := toolInputGuardrail.Fn(nil, nil, tool, json.RawMessage(`{"value":"tripwire-tool"}`))
	if err != nil || !toolIn.TripwireTriggered {
		t.Fatalf("tool input guardrail result = %+v err=%v", toolIn, err)
	}
	toolOut, err := toolOutputGuardrail.Fn(nil, nil, tool, agentsdk.ToolResult{Content: "tripwire-tool-output"})
	if err != nil || !toolOut.TripwireTriggered {
		t.Fatalf("tool output guardrail result = %+v err=%v", toolOut, err)
	}
}
