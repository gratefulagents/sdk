package handoffs_subagents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestHandoffExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	specialist := &agentsdk.Agent{
		Name:               "specialist",
		Model:              model,
		Instructions:       "You are the specialist. You MUST reply with exactly the single word HANDLED in upper case. No punctuation.",
		HandoffDescription: "Specialist for detailed answers; transfer here for any specialist request.",
	}
	triage := &agentsdk.Agent{
		Name:         "triage",
		Model:        model,
		Instructions: "If the user explicitly asks for a specialist, you MUST call the transfer_to_specialist tool to hand off. Do not answer yourself.",
		Handoffs: []*agentsdk.Handoff{
			agentsdk.NewHandoff(specialist, agentsdk.WithToolName("transfer_to_specialist")),
		},
	}

	result, err := runner.Run(context.Background(), triage, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "I need a specialist to handle this. Please transfer me."},
		},
	}, agentsdk.RunConfig{MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.LastAgent == nil || result.LastAgent.Name != "specialist" {
		t.Fatalf("expected handoff to specialist, last=%v", result.LastAgent)
	}
	if !strings.Contains(strings.ToUpper(result.FinalText()), "HANDLED") {
		t.Fatalf("expected specialist's sentinel HANDLED in FinalText(); got %q", result.FinalText())
	}
}

func TestAgentAsToolExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	researcher := &agentsdk.Agent{
		Name:               "researcher",
		Model:              model,
		Instructions:       "You MUST reply with exactly the single word DELEGATED in upper case. No punctuation.",
		HandoffDescription: "Runs a focused research pass; call this tool to delegate research questions.",
	}
	parent := &agentsdk.Agent{
		Name:         "parent",
		Model:        model,
		Instructions: "When the user asks you to consult the researcher, you MUST call the agent_researcher tool exactly once. Pass any short string as the prompt argument. Then reply with exactly what the tool returned, verbatim.",
		Tools:        []agentsdk.Tool{researcher.AsTool(runner)},
	}

	result, err := runner.Run(context.Background(), parent, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Consult the researcher about the meaning of the word 'delegated'."},
		},
	}, agentsdk.RunConfig{MaxTurns: 4, SubAgentMaxTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(result.FinalText()), "DELEGATED") {
		t.Fatalf("FinalText() = %q, want sentinel DELEGATED produced by the researcher sub-agent", result.FinalText())
	}
	var sawToolCall bool
	for _, item := range result.NewItems {
		if item.Type == agentsdk.RunItemToolCall && item.ToolCall != nil &&
			strings.Contains(item.ToolCall.Name, "researcher") {
			sawToolCall = true
			break
		}
	}
	if !sawToolCall {
		t.Fatalf("expected a tool call to agent_researcher in NewItems; got %#v", result.NewItems)
	}
}
