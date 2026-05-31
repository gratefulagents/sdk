package model_abstraction_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// countingProvider asserts that requests routed through "openai/<model>"
// actually reach the registered OpenAI provider — not some default fallback.
type countingProvider struct {
	inner agentsdk.ModelProvider
	calls atomic.Int32
}

func (c *countingProvider) GetModel(name string) (agentsdk.Model, error) {
	c.calls.Add(1)
	return c.inner.GetModel(name)
}

func (c *countingProvider) Close() error { return c.inner.Close() }

// TestModelAbstractionAndMultiProviderExample wires the live OpenAI provider
// behind a MultiProvider under the prefix "openai" and verifies that
// requests using the "openai/<model>" form are routed correctly. It also
// covers ParseModelPrefix, the public helper used to build that prefix.
func TestModelAbstractionAndMultiProviderExample(t *testing.T) {
	inner := liverunner.Provider(t)
	model := liverunner.DefaultModel(t)
	counter := &countingProvider{inner: inner}

	providers := agentsdk.NewMultiProvider("openai")
	providers.Register("openai", counter)

	runner := agentsdk.NewRunnerWithProvider(providers)
	prefixed := "openai/" + model
	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "router",
		Model:        prefixed,
		Instructions: "You MUST reply with exactly the single word PINEAPPLE in upper case. No punctuation.",
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Reply now."},
		},
	}, agentsdk.RunConfig{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(result.FinalText()), "PINEAPPLE") {
		t.Fatalf("FinalText() = %q, want sentinel PINEAPPLE", result.FinalText())
	}
	if counter.calls.Load() < 1 {
		t.Fatalf("openai/ prefix did not route to registered provider (calls=%d)", counter.calls.Load())
	}

	prefix, parsedModel := agentsdk.ParseModelPrefix("anthropic/claude-haiku-4-5")
	if prefix != "anthropic" || parsedModel != "claude-haiku-4-5" {
		t.Fatalf("ParseModelPrefix() = %q, %q", prefix, parsedModel)
	}
}
