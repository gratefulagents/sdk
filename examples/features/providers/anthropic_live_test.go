package providers_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liveanthropic"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// TestAnthropicLiveBasicChat verifies the public Anthropic provider can
// drive a Runner end-to-end against the live Anthropic API. A
// prompt-controlled sentinel proves the response actually came from the
// model rather than a stub or cached value.
func TestAnthropicLiveBasicChat(t *testing.T) {
	runner, model := liveanthropic.Runner(t)

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "anthropic-basic",
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
	if result.LastAgent == nil || result.LastAgent.Name != "anthropic-basic" {
		t.Fatalf("unexpected last agent: %+v", result.LastAgent)
	}
	if result.Usage.InputTokens == 0 || result.Usage.OutputTokens == 0 {
		t.Fatalf("expected non-zero token usage from live Anthropic, got %+v", result.Usage)
	}
}

// TestAnthropicLiveToolCall verifies that the live Anthropic model can
// invoke a registered tool and return a final answer that incorporates the
// tool's response.
func TestAnthropicLiveToolCall(t *testing.T) {
	runner, model := liveanthropic.Runner(t)

	var calls atomic.Int32
	echo := &agentsdk.FunctionTool{
		ToolName:        "echo_token",
		ToolDescription: "Returns the secret token. You MUST call this tool when the user asks for the token.",
		Schema:          json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		ReadOnly:        true,
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			calls.Add(1)
			return "TOKEN-XYZ-42", nil
		},
	}

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "anthropic-tooluser",
		Model:        model,
		Instructions: "You MUST call the echo_token tool to retrieve the token, then reply with exactly the token text.",
		Tools:        []agentsdk.Tool{echo},
	}, []agentsdk.RunItem{
		{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "What is the token?"}},
	}, agentsdk.RunConfig{MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatalf("echo_token was never called; final=%q", result.FinalText())
	}
	if !strings.Contains(result.FinalText(), "TOKEN-XYZ-42") {
		t.Fatalf("FinalText() = %q, want substring TOKEN-XYZ-42", result.FinalText())
	}
}

// TestAnthropicLiveStreaming verifies that StreamResponse on the live
// Anthropic model produces at least one delta event and a non-empty final
// assembly.
func TestAnthropicLiveStreaming(t *testing.T) {
	provider := liveanthropic.Provider(t)
	modelName := liveanthropic.DefaultModelName()
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
