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

func TestIsClaudeModelName(t *testing.T) {
	cases := map[string]bool{
		"claude-opus-4.8":            true,
		"claude-3.5-sonnet":          true,
		"copilot/claude-sonnet-4.5":  true,
		"anthropic/claude-haiku-4-5": true,
		"CLAUDE-OPUS":                true,
		"gpt-4.1":                    false,
		"copilot/gpt-4.1":            false,
		"o3-mini":                    false,
		"":                           false,
	}
	for name, want := range cases {
		if got := isClaudeModelName(name); got != want {
			t.Errorf("isClaudeModelName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCopilotAnthropicBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.githubcopilot.com":                   "https://api.githubcopilot.com",
		"https://api.githubcopilot.com/":                  "https://api.githubcopilot.com",
		"https://api.githubcopilot.com/chat/completions":  "https://api.githubcopilot.com",
		"https://api.githubcopilot.com/v1":                "https://api.githubcopilot.com",
		"https://host/v1/chat/completions":                "https://host",
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
	if got := model.Provider(); got != "anthropic" {
		t.Fatalf("Provider() = %q, want anthropic (Claude must route to the Anthropic endpoint)", got)
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
	if gotModel != "claude-opus-4.8" {
		t.Fatalf("model = %q, want claude-opus-4.8 (no name mangling)", gotModel)
	}
}

// TestCopilotRoutesNonClaudeToChatCompletions verifies non-Claude models still
// use the OpenAI chat-completions path.
func TestCopilotRoutesNonClaudeToChatCompletions(t *testing.T) {
	provider := newCopilotProviderFromSpec(ProviderSpec{
		Provider:        DefaultProviderCopilot,
		ProviderAPIKeys: map[string]string{DefaultProviderCopilot: "copilot-token"},
	})
	model, err := provider.GetModel("gpt-4.1")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != DefaultProviderCopilot {
		t.Fatalf("Provider() = %q, want %q", got, DefaultProviderCopilot)
	}
}

// TestCopilotClaudeViaChatEscapeHatch verifies the env override forces Claude
// back onto the chat-completions path.
func TestCopilotClaudeViaChatEscapeHatch(t *testing.T) {
	t.Setenv("GRATEFULAGENTS_COPILOT_CLAUDE_VIA_CHAT", "1")
	provider := newCopilotProviderFromSpec(ProviderSpec{
		Provider:        DefaultProviderCopilot,
		ProviderAPIKeys: map[string]string{DefaultProviderCopilot: "copilot-token"},
	})
	model, err := provider.GetModel("claude-opus-4.8")
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	if got := model.Provider(); got != DefaultProviderCopilot {
		t.Fatalf("Provider() = %q, want %q (escape hatch must use chat-completions)", got, DefaultProviderCopilot)
	}
}
