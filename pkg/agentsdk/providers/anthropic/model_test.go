package anthropic

import (
	"context"
	"strings"
	"testing"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
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

func TestBuildRequestAppliesPromptCacheBreakpoints(t *testing.T) {
	m := &AnthropicModel{model: "claude-fable-5", promptCaching: true, adaptiveThinking: true}
	req := agentsdk.ModelRequest{
		Model:        "claude-fable-5",
		Instructions: "system prompt",
		Input: []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "turn one"}},
			{Type: agentsdk.RunItemReasoning, Reasoning: &agentsdk.ReasoningData{Signature: "sig"}},
			{Type: agentsdk.RunItemToolCall, ToolCall: &agentsdk.ToolCallData{ID: "c1", Name: "read_file", Input: []byte(`{}`)}},
			{Type: agentsdk.RunItemToolOutput, ToolOutput: &agentsdk.ToolOutputData{CallID: "c1", Content: "result"}},
		},
	}
	apiReq := m.buildRequest(req)

	if n := len(apiReq.System); n == 0 || apiReq.System[n-1].CacheControl == nil {
		t.Fatal("last system block missing cache_control")
	}

	marked := 0
	var markedTypes []string
	for _, msg := range apiReq.Messages {
		for _, block := range msg.Content {
			if block.CacheControl != nil {
				marked++
				markedTypes = append(markedTypes, block.Type)
			}
			if block.CacheControl != nil && (block.Type == "thinking" || block.Type == "redacted_thinking") {
				t.Fatalf("cache_control set on %s block", block.Type)
			}
		}
	}
	if marked != 2 {
		t.Fatalf("marked message blocks = %d (%v), want 2", marked, markedTypes)
	}
	// Last two cacheable positions: the tool_result and the tool_use before it
	// (the reasoning item between them cannot carry cache_control).
	if markedTypes[0] != "tool_use" || markedTypes[1] != "tool_result" {
		t.Fatalf("marked types = %v, want [tool_use tool_result]", markedTypes)
	}
}

func TestBuildRequestPromptCachingDisabledByDefault(t *testing.T) {
	m := &AnthropicModel{model: "claude-fable-5"}
	apiReq := m.buildRequest(agentsdk.ModelRequest{
		Model:        "claude-fable-5",
		Instructions: "system prompt",
		Input:        []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	})
	for _, sys := range apiReq.System {
		if sys.CacheControl != nil {
			t.Fatal("cache_control set without opt-in")
		}
	}
}

func TestBuildRequestDefaultMaxTokens(t *testing.T) {
	m := &AnthropicModel{model: "claude-fable-5", defaultMaxTokens: 64000}
	apiReq := m.buildRequest(agentsdk.ModelRequest{Model: "claude-fable-5"})
	if apiReq.MaxTokens != 64000 {
		t.Fatalf("MaxTokens = %d, want 64000", apiReq.MaxTokens)
	}
	explicit := m.buildRequest(agentsdk.ModelRequest{Model: "claude-fable-5", Settings: agentsdk.ModelSettings{MaxTokens: 1024}})
	if explicit.MaxTokens != 1024 {
		t.Fatalf("MaxTokens = %d, want explicit 1024", explicit.MaxTokens)
	}
	fallback := (&AnthropicModel{model: "claude-fable-5"}).buildRequest(agentsdk.ModelRequest{Model: "claude-fable-5"})
	if fallback.MaxTokens != 16384 {
		t.Fatalf("MaxTokens = %d, want 16384 default", fallback.MaxTokens)
	}
}

func TestItemsToMessagesRoutesPDFToDocumentBlock(t *testing.T) {
	msgs := itemsToAnthropicMessages([]agentsdk.RunItem{{
		Type: agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{
			Text: "see attachment",
			Images: []agentsdk.ImageAttachment{
				{MediaType: "application/pdf", Data: "cGRm"},
				{MediaType: "image/png", Data: "cG5n"},
			},
		},
	}})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	var types []string
	for _, block := range msgs[0].Content {
		types = append(types, block.Type)
	}
	want := []string{"text", "document", "image"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("block types = %v, want %v", types, want)
	}
}

func TestBuildRequestOAuthPrependsClaudeCodeIdentity(t *testing.T) {
	m, err := newAnthropicModel(anthropicModelConfig{
		apiKey:   "sk-ant-oat01-test",
		authMode: "oauth",
	})
	if err != nil {
		t.Fatalf("newAnthropicModel() error = %v", err)
	}
	req := m.buildRequest(agentsdk.ModelRequest{
		Model:        "claude-sonnet-4-5",
		Instructions: "Be helpful.",
	})
	if len(req.System) != 2 {
		t.Fatalf("System blocks = %d, want 2", len(req.System))
	}
	if req.System[0].Text != internalanthropic.ClaudeCodeIdentity {
		t.Fatalf("first system block = %q, want Claude Code identity", req.System[0].Text)
	}
	if req.System[1].Text != "Be helpful." {
		t.Fatalf("second system block = %q, want instructions", req.System[1].Text)
	}

	// Identity block present even without instructions.
	req = m.buildRequest(agentsdk.ModelRequest{Model: "claude-sonnet-4-5"})
	if len(req.System) != 1 || req.System[0].Text != internalanthropic.ClaudeCodeIdentity {
		t.Fatalf("System without instructions = %+v, want identity only", req.System)
	}
}

func TestBuildRequestAPIKeyOmitsClaudeCodeIdentity(t *testing.T) {
	m, err := newAnthropicModel(anthropicModelConfig{apiKey: "sk-ant-api-test"})
	if err != nil {
		t.Fatalf("newAnthropicModel() error = %v", err)
	}
	req := m.buildRequest(agentsdk.ModelRequest{
		Model:        "claude-sonnet-4-5",
		Instructions: "Be helpful.",
	})
	if len(req.System) != 1 || req.System[0].Text != "Be helpful." {
		t.Fatalf("System = %+v, want instructions only", req.System)
	}
}
