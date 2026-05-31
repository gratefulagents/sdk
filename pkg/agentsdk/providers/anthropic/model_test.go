package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestAnthropicProviderWithConfigUsesSuppliedAPIKey(t *testing.T) {
	provider := NewProviderWithConfig(ProviderConfig{
		APIKey:  "anthropic-key",
		BaseURL: "http://localhost:8080",
	})
	model, err := provider.GetModel("small")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	anthropicModel, ok := model.(*AnthropicModel)
	if !ok {
		t.Fatalf("model type = %T, want *AnthropicModel", model)
	}
	if anthropicModel.model != "claude-haiku-4-5" {
		t.Fatalf("model = %q, want claude-haiku-4-5", anthropicModel.model)
	}
}

func TestAnthropicModelWithNilClientReturnsConfigurationError(t *testing.T) {
	model := NewAnthropicModelWithClient(nil)
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("GetResponse() error = %v, want configuration error", err)
	}
	if _, err := model.StreamResponse(context.Background(), agentsdk.ModelRequest{}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("StreamResponse() error = %v, want configuration error", err)
	}
}
