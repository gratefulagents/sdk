package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestNormalizeCopilotModelName(t *testing.T) {
	cases := map[string]string{
		"copilot/claude-sonnet-4.5":  "claude-sonnet-4.5",
		"copilot/gpt-4.1":            "gpt-4.1",
		"anthropic/claude-haiku-4-5": "anthropic/claude-haiku-4-5",
		"gpt-4.1":                    "gpt-4.1",
		"":                           "",
	}
	for name, want := range cases {
		if got := normalizeCopilotModelName(name); got != want {
			t.Errorf("normalizeCopilotModelName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCopilotAnthropicBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.individual.githubcopilot.com":                  "https://api.individual.githubcopilot.com",
		"https://api.individual.githubcopilot.com/":                 "https://api.individual.githubcopilot.com",
		"https://api.individual.githubcopilot.com/chat/completions": "https://api.individual.githubcopilot.com",
		"https://api.individual.githubcopilot.com/v1":               "https://api.individual.githubcopilot.com",
		"https://api.githubcopilot.com":                             "https://api.githubcopilot.com",
		"https://host/v1/chat/completions":                          "https://host",
	}
	for in, want := range cases {
		if got := copilotAnthropicBaseURL(in); got != want {
			t.Errorf("copilotAnthropicBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCopilotRoutesClaudeToAnthropicMessages verifies that a Claude model
// served through Copilot is sent to the Anthropic /v1/messages endpoint with
// Copilot bearer auth + integration headers, instead of chat-completions.
func TestCopilotRoutesClaudeToAnthropicMessages(t *testing.T) {
	var gotPath, gotModel string
	gotHeaders := http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPath = r.URL.Path
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4.8","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:         DefaultProviderCopilot,
		Model:            "claude-opus-4.8",
		ProviderAPIKeys:  map[string]string{DefaultProviderCopilot: "copilot-token"},
		ProviderBaseURLs: map[string]string{DefaultProviderCopilot: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("claude-opus-4.8")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != DefaultProviderCopilot {
		t.Fatalf("Provider() = %q, want %q", got, DefaultProviderCopilot)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model: "claude-opus-4.8",
		Input: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if !strings.HasSuffix(gotPath, "/v1/messages") {
		t.Fatalf("path = %q, want suffix /v1/messages", gotPath)
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer copilot-token" {
		t.Fatalf("Authorization = %q, want Bearer copilot-token", got)
	}
	if got := gotHeaders.Get("Copilot-Integration-Id"); got != "vscode-chat" {
		t.Fatalf("Copilot-Integration-Id = %q, want vscode-chat", got)
	}
	if got := gotHeaders.Get("anthropic-beta"); got != copilotAnthropicBeta {
		t.Fatalf("anthropic-beta = %q, want %q (interleaved-thinking, per opencode)", got, copilotAnthropicBeta)
	}
	if got := gotHeaders.Get("X-GitHub-Api-Version"); got != copilotGitHubAPIVersion {
		t.Fatalf("X-GitHub-Api-Version = %q, want %q", got, copilotGitHubAPIVersion)
	}
	if got := gotHeaders.Get("X-Initiator"); got != "user" {
		t.Fatalf("X-Initiator = %q, want user", got)
	}
	if gotModel != "claude-opus-4.8" {
		t.Fatalf("model = %q, want claude-opus-4.8 (no name mangling)", gotModel)
	}
}

// TestCopilotClaudeUsesAdaptiveThinking verifies the Copilot Claude path sends
// thinking.type=adaptive + output_config.effort (mapped from reasoning effort)
// instead of thinking.type=enabled, which Copilot's /v1/messages shim rejects.
func TestCopilotClaudeUsesAdaptiveThinking(t *testing.T) {
	var body struct {
		Thinking     map[string]any `json:"thinking"`
		OutputConfig map[string]any `json:"output_config"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4.8","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:         DefaultProviderCopilot,
		Model:            "claude-opus-4.8",
		ProviderAPIKeys:  map[string]string{DefaultProviderCopilot: "copilot-token"},
		ProviderBaseURLs: map[string]string{DefaultProviderCopilot: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("claude-opus-4.8")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model:    "claude-opus-4.8",
		Settings: agentsdk.ModelSettings{ThinkingBudget: 8000, ReasoningEffort: "xhigh"},
		Input:    []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if got, _ := body.Thinking["type"].(string); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive (enabled is rejected by Copilot)", got)
	}
	if _, hasBudget := body.Thinking["budget_tokens"]; hasBudget {
		t.Fatalf("thinking must not carry budget_tokens on the Copilot adaptive path: %v", body.Thinking)
	}
	if got, _ := body.OutputConfig["effort"].(string); got != "max" {
		t.Fatalf("output_config.effort = %q, want max (host xhigh maps to Anthropic max; xhigh is rejected by some Claude models)", got)
	}
}

// TestCopilotRoutesGPT4ToChatCompletions verifies GPT-4-era OpenAI models use
// the Copilot OpenAI-shaped chat-completions path.
func TestCopilotRoutesGPT4ToChatCompletions(t *testing.T) {
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_test","object":"chat.completion","created":0,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:         DefaultProviderCopilot,
		Model:            "gpt-4.1",
		ProviderAPIKeys:  map[string]string{DefaultProviderCopilot: "copilot-token"},
		ProviderBaseURLs: map[string]string{DefaultProviderCopilot: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("gpt-4.1")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != DefaultProviderCopilot {
		t.Fatalf("Provider() = %q, want %q", got, DefaultProviderCopilot)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model: "gpt-4.1",
		Input: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotModel != "gpt-4.1" {
		t.Fatalf("model = %q, want gpt-4.1", gotModel)
	}
}

func TestCopilotRoutesGPT5ToResponses(t *testing.T) {
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.2","usage":{"input_tokens":1}}}`,
			`{"type":"response.content_part.added","output_index":0,"part":{"type":"output_text"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"ok"}`,
			`{"type":"response.output_text.done","output_index":0}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.2","usage":{"input_tokens":1,"output_tokens":1},"output":[]}}`,
		} {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:         DefaultProviderCopilot,
		Model:            "gpt-5.2",
		ProviderAPIKeys:  map[string]string{DefaultProviderCopilot: "copilot-token"},
		ProviderBaseURLs: map[string]string{DefaultProviderCopilot: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("gpt-5.2")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != DefaultProviderCopilot {
		t.Fatalf("Provider() = %q, want %q", got, DefaultProviderCopilot)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model: "gpt-5.2",
		Input: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gotModel != "gpt-5.2" {
		t.Fatalf("model = %q, want gpt-5.2", gotModel)
	}
}

// TestCopilotClaudeDefaultsToMessages verifies Claude on Copilot uses the
// Anthropic-shaped /v1/messages path by default.
func TestCopilotClaudeDefaultsToMessages(t *testing.T) {
	provider := newCopilotProviderFromSpec(ProviderSpec{
		Provider:        DefaultProviderCopilot,
		ProviderAPIKeys: map[string]string{DefaultProviderCopilot: "copilot-token"},
	})
	model, err := provider.GetModel("claude-opus-4.8")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != DefaultProviderCopilot {
		t.Fatalf("Provider() = %q, want %q", got, DefaultProviderCopilot)
	}
}

// TestCopilotChatCompletionsEscapeHatch verifies the rollback env override
// restores the previous chat-completions transport.
func TestCopilotChatCompletionsEscapeHatch(t *testing.T) {
	t.Setenv("GRATEFULAGENTS_COPILOT_CHAT_COMPLETIONS", "1")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_test","object":"chat.completion","created":0,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:         DefaultProviderCopilot,
		ProviderAPIKeys:  map[string]string{DefaultProviderCopilot: "copilot-token"},
		ProviderBaseURLs: map[string]string{DefaultProviderCopilot: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("gpt-4.1")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model: "gpt-4.1",
		Input: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
}

// TestCopilotClaude45UsesEnabledThinking locks in the restored thinking path
// for the budget-tokens-only Claude generation on Copilot's /v1/messages shim
// (claude-haiku-4.5 / claude-sonnet-4.5 / claude-opus-4.5): these models
// return no thinking blocks for thinking.type=adaptive, and Copilot upstream
// verifies thinking.type=enabled + budget_tokens end-to-end (see
// BerriAI/litellm#28053). Only the 4.6+/fable/5.x generations use adaptive.
func TestCopilotClaude45UsesEnabledThinking(t *testing.T) {
	var body struct {
		Thinking     map[string]any `json:"thinking"`
		OutputConfig map[string]any `json:"output_config"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4.5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	provider, err := NewProviderFromConfig(ProviderSpec{
		Provider:         DefaultProviderCopilot,
		Model:            "claude-sonnet-4.5",
		ProviderAPIKeys:  map[string]string{DefaultProviderCopilot: "copilot-token"},
		ProviderBaseURLs: map[string]string{DefaultProviderCopilot: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel("claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.GetResponse(context.Background(), agentsdk.ModelRequest{
		Model:    "claude-sonnet-4.5",
		Settings: agentsdk.ModelSettings{ThinkingBudget: 8192, ReasoningEffort: "high"},
		Input:    []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}}},
	}); err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if got, _ := body.Thinking["type"].(string); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled (adaptive yields no thinking blocks on the 4.5 family)", got)
	}
	if got, _ := body.Thinking["budget_tokens"].(float64); int(got) != 8192 {
		t.Fatalf("thinking.budget_tokens = %v, want 8192", body.Thinking["budget_tokens"])
	}
	if len(body.OutputConfig) != 0 {
		t.Fatalf("output_config must be empty on the enabled path: %v", body.OutputConfig)
	}
}
