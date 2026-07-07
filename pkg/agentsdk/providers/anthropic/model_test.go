package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestBuildRequestThinkingShapePerModel pins the per-model thinking request
// shape. On effort-first gateways (adaptiveThinking, e.g. Copilot's
// /v1/messages shim) the 4.5-and-older Claude generations must keep
// thinking.type=enabled + budget_tokens — the shim returns no thinking blocks
// for adaptive requests against them — while 4.6+/fable/5.x use adaptive +
// output_config.effort. On the first-party API only the effort-only models
// (fable, opus 4.7+, generation 5) switch to adaptive.
func TestBuildRequestThinkingShapePerModel(t *testing.T) {
	cases := []struct {
		name       string
		adaptive   bool // provider AdaptiveThinking (effort-first gateway)
		model      string
		settings   agentsdk.ModelSettings
		wantType   string // "" = no thinking config
		wantBudget int
		wantEffort string
	}{
		{
			name:       "copilot 4.5 generation keeps enabled+budget",
			adaptive:   true,
			model:      "claude-sonnet-4.5",
			settings:   agentsdk.ModelSettings{ThinkingBudget: 8192, ReasoningEffort: "high"},
			wantType:   "enabled",
			wantBudget: 8192,
		},
		{
			name:       "copilot 4.5 with effort only derives a budget",
			adaptive:   true,
			model:      "claude-haiku-4.5",
			settings:   agentsdk.ModelSettings{ReasoningEffort: "medium"},
			wantType:   "enabled",
			wantBudget: 4096,
		},
		{
			name:       "copilot 4.6+ uses adaptive+effort",
			adaptive:   true,
			model:      "claude-opus-4.8",
			settings:   agentsdk.ModelSettings{ThinkingBudget: 8192, ReasoningEffort: "xhigh"},
			wantType:   "adaptive",
			wantEffort: "max",
		},
		{
			name:       "copilot adaptive with budget only defaults to medium effort",
			adaptive:   true,
			model:      "claude-fable-5",
			settings:   agentsdk.ModelSettings{ThinkingBudget: 8192},
			wantType:   "adaptive",
			wantEffort: "medium",
		},
		{
			name:       "first-party 4.5 keeps enabled+budget",
			adaptive:   false,
			model:      "claude-sonnet-4-5",
			settings:   agentsdk.ModelSettings{ThinkingBudget: 4096, ReasoningEffort: "medium"},
			wantType:   "enabled",
			wantBudget: 4096,
		},
		{
			name:       "first-party 4.6 still accepts budget so keeps enabled",
			adaptive:   false,
			model:      "claude-sonnet-4-6",
			settings:   agentsdk.ModelSettings{ThinkingBudget: 4096, ReasoningEffort: "medium"},
			wantType:   "enabled",
			wantBudget: 4096,
		},
		{
			name:       "first-party effort-only model switches to adaptive",
			adaptive:   false,
			model:      "claude-fable-5",
			settings:   agentsdk.ModelSettings{ThinkingBudget: 4096},
			wantType:   "adaptive",
			wantEffort: "medium",
		},
		{
			name:       "first-party opus 4.7 is effort-only",
			adaptive:   false,
			model:      "claude-opus-4-7",
			settings:   agentsdk.ModelSettings{ThinkingBudget: 8192, ReasoningEffort: "high"},
			wantType:   "adaptive",
			wantEffort: "high",
		},
		{
			name:     "reasoning none without budget disables thinking",
			adaptive: true,
			model:    "claude-sonnet-4.5",
			settings: agentsdk.ModelSettings{ReasoningEffort: "none"},
			wantType: "",
		},
		{
			name:     "no reasoning settings sends no thinking config",
			adaptive: false,
			model:    "claude-sonnet-4-5",
			wantType: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &AnthropicModel{model: tc.model, adaptiveThinking: tc.adaptive}
			apiReq := m.buildRequest(agentsdk.ModelRequest{Model: tc.model, Settings: tc.settings})
			if tc.wantType == "" {
				if apiReq.Thinking != nil {
					t.Fatalf("Thinking = %+v, want none", apiReq.Thinking)
				}
				return
			}
			if apiReq.Thinking == nil || apiReq.Thinking.Type != tc.wantType {
				t.Fatalf("Thinking = %+v, want type %q", apiReq.Thinking, tc.wantType)
			}
			if apiReq.Thinking.BudgetTokens != tc.wantBudget {
				t.Fatalf("budget_tokens = %d, want %d", apiReq.Thinking.BudgetTokens, tc.wantBudget)
			}
			if apiReq.OutputEffort != tc.wantEffort {
				t.Fatalf("output_config.effort = %q, want %q", apiReq.OutputEffort, tc.wantEffort)
			}
		})
	}
}

// TestThinkingShapeFlipOn400 verifies the self-healing retry: when the API
// rejects the derived thinking shape with the thinking.type 400, the request
// is retried once with the opposite shape and the working shape sticks for
// subsequent requests from the same model handle.
func TestThinkingShapeFlipOn400(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.enabled\" is not supported for this model. Use \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	m, err := newAnthropicModel(anthropicModelConfig{apiKey: "test-key", baseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m.model = "claude-sonnet-4-5" // resolves to enabled+budget

	req := agentsdk.ModelRequest{
		Model:    "claude-sonnet-4-5",
		Settings: agentsdk.ModelSettings{ThinkingBudget: 4096, ReasoningEffort: "medium"},
		Input:    []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	}
	resp, err := m.GetResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2 (original + flipped retry)", len(bodies))
	}
	first, _ := bodies[0]["thinking"].(map[string]any)
	if got, _ := first["type"].(string); got != "enabled" {
		t.Fatalf("first thinking.type = %q, want enabled", got)
	}
	second, _ := bodies[1]["thinking"].(map[string]any)
	if got, _ := second["type"].(string); got != "adaptive" {
		t.Fatalf("retry thinking.type = %q, want adaptive", got)
	}
	if _, hasBudget := second["budget_tokens"]; hasBudget {
		t.Fatalf("retry thinking must not carry budget_tokens: %v", second)
	}
	if resp == nil {
		t.Fatal("GetResponse() = nil response after flipped retry")
	}

	// The working shape sticks: the next request goes straight to adaptive.
	if apiReq := m.buildRequest(req); apiReq.Thinking == nil || apiReq.Thinking.Type != "adaptive" {
		t.Fatalf("post-flip Thinking = %+v, want adaptive", apiReq.Thinking)
	}
}

// TestThinkingShapeFlipIgnoresUnrelated400 ensures ordinary bad-request errors
// are not retried with a different thinking shape.
func TestThinkingShapeFlipIgnoresUnrelated400(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is too large"}}`))
	}))
	defer srv.Close()

	m, err := newAnthropicModel(anthropicModelConfig{apiKey: "test-key", baseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m.model = "claude-sonnet-4-5"

	_, err = m.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model:    "claude-sonnet-4-5",
		Settings: agentsdk.ModelSettings{ThinkingBudget: 4096},
		Input:    []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	})
	if err == nil {
		t.Fatal("GetResponse() = nil error, want 400 passthrough")
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want 1 (no shape-flip retry)", calls)
	}
}
