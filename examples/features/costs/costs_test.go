package costs_test

import (
	"context"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestCostsExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "costed",
		Model:        model,
		Instructions: "Reply with a single short sentence.",
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Say hello."},
		},
	}, agentsdk.RunConfig{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.InputTokens == 0 && result.Usage.OutputTokens == 0 {
		t.Fatalf("expected non-zero token usage, got %+v", result.Usage)
	}
	if len(result.RawResponses) == 0 {
		t.Fatal("expected at least one raw response")
	}

	// Exercise the public cost-estimator surface against the real usage we got
	// back from the live model. We don't pin a specific dollar amount because
	// the live model name and price table can drift; we just assert that the
	// estimator returns a positive number for known models.
	openAICost, known := sdkopenai.EstimateCost("gpt-4.1-mini", result.Usage)
	if !known || openAICost <= 0 {
		t.Fatalf("OpenAI cost estimate failed: cost=%f known=%v", openAICost, known)
	}
	if anthropicCost := sdkanthropic.CalculateCost("claude-haiku-4-5", result.Usage); anthropicCost <= 0 {
		t.Fatalf("Anthropic cost estimate failed: cost=%f", anthropicCost)
	}
}
