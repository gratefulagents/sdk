package openai

import (
	"context"
	"math"
	"strings"
	"testing"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	internalopenai "github.com/gratefulagents/sdk/internal/openai"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// TestDowngradeEffortOnError pins the one-step effort ladder used when a model
// rejects the requested reasoning effort value: max→xhigh→high and
// none→minimal, applied only to 400s that mention the reasoning effort.
func TestDowngradeEffortOnError(t *testing.T) {
	m := &OpenAIModel{}
	effortErr := &internalopenai.RequestError{
		StatusCode: 400,
		Body:       `{"error":{"code":"invalid_reasoning_effort","message":"Invalid reasoning effort: not supported for this model"}}`,
	}

	sent := internalanthropic.CreateMessageRequest{Model: "gpt-5.1", ReasoningEffort: "max"}
	healed, ok := m.downgradeEffortOnError(effortErr, sent)
	if !ok || healed.ReasoningEffort != "xhigh" {
		t.Fatalf("downgrade(max) = %q ok=%v, want xhigh", healed.ReasoningEffort, ok)
	}
	healed, ok = m.downgradeEffortOnError(effortErr, healed)
	if !ok || healed.ReasoningEffort != "high" {
		t.Fatalf("downgrade(xhigh) = %q ok=%v, want high", healed.ReasoningEffort, ok)
	}
	if _, ok := m.downgradeEffortOnError(effortErr, healed); ok {
		t.Fatal("downgrade(high) should not retry: high is universally supported")
	}

	sent.ReasoningEffort = "none"
	healed, ok = m.downgradeEffortOnError(effortErr, sent)
	if !ok || healed.ReasoningEffort != "minimal" {
		t.Fatalf("downgrade(none) = %q ok=%v, want minimal", healed.ReasoningEffort, ok)
	}

	sent.ReasoningEffort = "max"
	unrelated := &internalopenai.RequestError{StatusCode: 400, Body: "max_tokens is too large"}
	if _, ok := m.downgradeEffortOnError(unrelated, sent); ok {
		t.Fatal("unrelated 400 must not trigger an effort downgrade")
	}
	server := &internalopenai.RequestError{StatusCode: 500, Body: "reasoning effort backend error"}
	if _, ok := m.downgradeEffortOnError(server, sent); ok {
		t.Fatal("non-400 errors must not trigger an effort downgrade")
	}
	sent.ReasoningEffort = ""
	if _, ok := m.downgradeEffortOnError(effortErr, sent); ok {
		t.Fatal("requests without an effort must not be retried")
	}
}

type rawCompactionFixture string

func (r rawCompactionFixture) RawJSON() string { return string(r) }

func TestOpenAIModelEstimateCostUsesCachedInputPricing(t *testing.T) {
	model := &OpenAIModel{model: "gpt-5.5"}

	got, known := model.EstimateCost(agentsdk.Usage{
		InputTokens:     20_168_127,
		OutputTokens:    42_492,
		CacheReadTokens: 12_400_000,
	})

	const want = 46.315395
	if !known || math.Abs(got-want) > 1e-9 {
		t.Fatalf("EstimateCost() = (%f, %t), want (%f, true)", got, known, want)
	}
}

func TestOpenAIProviderWithConfigUsesSuppliedAPIKey(t *testing.T) {
	provider := NewProviderWithConfig(ProviderConfig{
		BaseURL: "http://localhost:11434/v1",
		APIKey:  "local-key",
	})
	model, err := provider.GetModel("local-model")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	openAIModel, ok := model.(*OpenAIModel)
	if !ok {
		t.Fatalf("model type = %T, want *OpenAIModel", model)
	}
	if openAIModel.model != "local-model" {
		t.Fatalf("model = %q, want local-model", openAIModel.model)
	}
}

func TestOpenAIModelWithNilClientReturnsConfigurationError(t *testing.T) {
	model := NewModelWithClient(nil)
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("GetResponse() error = %v, want configuration error", err)
	}
	if _, err := model.StreamResponse(context.Background(), agentsdk.ModelRequest{}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("StreamResponse() error = %v, want configuration error", err)
	}
}

func TestSummarizeProviderCompactionOutputPreservesFullText(t *testing.T) {
	fullText := strings.Repeat("implemented tauri canonical source; ", 80) + "sentinel-end"

	summary := summarizeProviderCompactionOutput("resp_full", []agentsdk.RunItem{
		{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: fullText}},
		{Type: agentsdk.RunItemToolOutput, ToolOutput: &agentsdk.ToolOutputData{CallID: "call_1", Content: fullText}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID:               "cmp_1",
			EncryptedContent: "encrypted-state-sentinel",
			CreatedBy:        "openai",
		}},
	})

	if !strings.Contains(summary, fullText) {
		t.Fatalf("summary did not preserve full text; got %q", summary)
	}
	if !strings.Contains(summary, "encrypted-state-sentinel") {
		t.Fatalf("summary did not preserve encrypted compaction content; got %q", summary)
	}
}

func TestSummarizeProviderCompactionResponseUsesRawJSON(t *testing.T) {
	raw := rawCompactionFixture(`{"id":"resp_raw","output":[{"type":"message","content":[{"type":"output_text","text":"raw-sentinel-full-output"}]}]}`)

	summary := summarizeProviderCompactionResponse("resp_fallback", raw, []agentsdk.RunItem{
		{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "fallback text"}},
	})

	if !strings.Contains(summary, "raw-sentinel-full-output") {
		t.Fatalf("summary = %q, want raw provider output", summary)
	}
	if strings.Contains(summary, "fallback text") {
		t.Fatalf("summary = %q, did not expect normalized fallback when raw JSON is present", summary)
	}
}
