package providers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liveopenrouter"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// TestOpenRouterLiveBasicChat verifies that the OpenAI-compatible provider
// can drive a Runner against OpenRouter's live API. OpenRouter exposes an
// OpenAI-compatible chat-completions endpoint, so the same sdkopenai
// provider works with a custom BaseURL.
func TestOpenRouterLiveBasicChat(t *testing.T) {
	runner, model := liveopenrouter.Runner(t)

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "openrouter-basic",
		Model:        model,
		Instructions: "You MUST reply with exactly the single word PINEAPPLE in upper case. No punctuation.",
	}, []agentsdk.RunItem{
		{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Reply now."}},
	}, agentsdk.RunConfig{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(result.FinalText()), "PINEAPPLE") {
		t.Fatalf("FinalText() = %q, want sentinel PINEAPPLE", result.FinalText())
	}
	if result.Usage.InputTokens == 0 || result.Usage.OutputTokens == 0 {
		t.Fatalf("expected non-zero token usage from OpenRouter, got %+v", result.Usage)
	}
}

// TestOpenRouterLiveStreaming verifies that StreamResponse against
// OpenRouter produces incremental deltas and assembled text.
func TestOpenRouterLiveStreaming(t *testing.T) {
	provider := liveopenrouter.Provider(t)
	modelName := liveopenrouter.DefaultModelName()
	model, err := provider.GetModel(modelName)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := model.StreamResponse(context.Background(), agentsdk.ModelRequest{
		Model:        modelName,
		Instructions: "Reply with three short facts about water.",
		Input: []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Tell me three short facts about water."}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		deltas    int
		assembled strings.Builder
	)
	for ev := range stream.Events {
		if ev.Type == agentsdk.ModelStreamDelta {
			deltas++
			assembled.WriteString(ev.Delta)
		}
	}
	if deltas == 0 {
		t.Fatalf("expected at least one delta event")
	}
	if strings.TrimSpace(assembled.String()) == "" {
		t.Fatalf("assembled deltas empty")
	}
}
