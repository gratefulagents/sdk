package openai

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

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
