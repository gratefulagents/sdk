package providers_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liveanthropic"
	"github.com/gratefulagents/sdk/examples/features/internal/liveopenai"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// countingProvider wraps a ModelProvider and increments calls when GetModel
// is invoked. It lets tests assert which inner provider a MultiProvider
// actually routed to, instead of trusting "the request didn't error".
type countingProvider struct {
	inner agentsdk.ModelProvider
	calls atomic.Int32
}

func (c *countingProvider) GetModel(name string) (agentsdk.Model, error) {
	c.calls.Add(1)
	return c.inner.GetModel(name)
}

func (c *countingProvider) Close() error { return c.inner.Close() }

// TestMultiProviderOpenAIPlusAnthropicLive registers the live OpenAI OAuth
// provider and the live Anthropic provider behind a single MultiProvider and
// runs two agents — one routed by "openai/<model>" prefix and one by
// "anthropic/<model>" prefix — sharing the same Runner.
//
// This exercises the public ParseModelPrefix routing the SDK ships, and is
// the canonical "multi-vendor in one app" wiring users care about. Each
// sub-test asserts not just that the call succeeded but that:
//   - the model produced text containing a prompt-controlled sentinel
//     (proves the request actually reached an LLM, not a stub), and
//   - GetModel hit only the expected inner provider (proves the prefix
//     routing in MultiProvider works correctly).
func TestMultiProviderOpenAIPlusAnthropicLive(t *testing.T) {
	openaiInner := liveopenai.Provider(t)
	anthropicInner := liveanthropic.Provider(t)
	openaiModel := liveopenai.DefaultModel
	anthropicModel := liveanthropic.DefaultModelName()

	openaiCounter := &countingProvider{inner: openaiInner}
	anthropicCounter := &countingProvider{inner: anthropicInner}

	mp := agentsdk.NewMultiProvider("openai")
	mp.Register("openai", openaiCounter)
	mp.Register("anthropic", anthropicCounter)

	runner := agentsdk.NewRunnerWithProvider(mp)

	t.Run("OpenAI route", func(t *testing.T) {
		before := anthropicCounter.calls.Load()
		prefixed := "openai/" + openaiModel
		result, err := runner.Run(context.Background(), &agentsdk.Agent{
			Name:         "router-openai",
			Model:        prefixed,
			Instructions: "You MUST reply with exactly the single word PINEAPPLE in upper case. No punctuation.",
		}, []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Reply now."}},
		}, agentsdk.RunConfig{MaxTurns: 1})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.ToUpper(result.FinalText()), "PINEAPPLE") {
			t.Fatalf("OpenAI route FinalText() = %q, want sentinel PINEAPPLE", result.FinalText())
		}
		if openaiCounter.calls.Load() < 1 {
			t.Fatalf("openai/ prefix did not route to OpenAI provider (calls=%d)", openaiCounter.calls.Load())
		}
		if got := anthropicCounter.calls.Load(); got != before {
			t.Fatalf("openai/ prefix unexpectedly hit Anthropic provider (calls delta=%d)", got-before)
		}
	})

	t.Run("Anthropic route", func(t *testing.T) {
		before := openaiCounter.calls.Load()
		prefixed := "anthropic/" + anthropicModel
		result, err := runner.Run(context.Background(), &agentsdk.Agent{
			Name:         "router-anthropic",
			Model:        prefixed,
			Instructions: "You MUST reply with exactly the single word MANGO in upper case. No punctuation.",
		}, []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Reply now."}},
		}, agentsdk.RunConfig{MaxTurns: 1})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.ToUpper(result.FinalText()), "MANGO") {
			t.Fatalf("Anthropic route FinalText() = %q, want sentinel MANGO", result.FinalText())
		}
		if anthropicCounter.calls.Load() < 1 {
			t.Fatalf("anthropic/ prefix did not route to Anthropic provider (calls=%d)", anthropicCounter.calls.Load())
		}
		if got := openaiCounter.calls.Load(); got != before {
			t.Fatalf("anthropic/ prefix unexpectedly hit OpenAI provider (calls delta=%d)", got-before)
		}
	})

	prefix, parsedModel := agentsdk.ParseModelPrefix("anthropic/" + anthropicModel)
	if prefix != "anthropic" || parsedModel != anthropicModel {
		t.Fatalf("ParseModelPrefix() = %q, %q", prefix, parsedModel)
	}
}
