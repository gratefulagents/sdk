package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/internal/anthropic"
)

func TestWithModelFallbacks(t *testing.T) {
	tests := []struct {
		name      string
		primary   string
		fallbacks []string
		want      []string
	}{
		{name: "no fallbacks returns nil", primary: "a", fallbacks: nil, want: nil},
		{
			name:      "primary first then fallbacks",
			primary:   "openrouter/qwen/qwen3-coder:free",
			fallbacks: []string{"openrouter/deepseek/deepseek-chat", "openrouter/openrouter/auto"},
			want: []string{
				"openrouter/qwen/qwen3-coder:free",
				"openrouter/deepseek/deepseek-chat",
				"openrouter/openrouter/auto",
			},
		},
		{
			name:      "dedupes and drops blanks",
			primary:   "a",
			fallbacks: []string{" ", "a", "b", "b"},
			want:      []string{"a", "b"},
		},
		{
			name:      "single distinct model returns nil",
			primary:   "a",
			fallbacks: []string{"a", "  "},
			want:      nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withModelFallbacks(tc.primary, tc.fallbacks)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestChatCompletionRequestOmitsEmptyModels(t *testing.T) {
	body, err := json.Marshal(chatCompletionRequest{Model: "a", Messages: []chatMessage{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); strings.Contains(got, "\"models\"") {
		t.Fatalf("empty models must be omitted, got %s", got)
	}

	body, err = json.Marshal(chatCompletionRequest{Model: "a", Models: []string{"a", "b"}, Messages: []chatMessage{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); !strings.Contains(got, "\"models\":[\"a\",\"b\"]") {
		t.Fatalf("expected models array in body, got %s", got)
	}
}

func TestChatCompletionsSendsModelsArrayWithFallbacks(t *testing.T) {
var captured map[string]any
server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
if r.URL.Path != "/v1/chat/completions" {
t.Fatalf("path = %q, want /v1/chat/completions", r.URL.Path)
}
raw, err := io.ReadAll(r.Body)
if err != nil {
t.Fatalf("read body: %v", err)
}
if err := json.Unmarshal(raw, &captured); err != nil {
t.Fatalf("unmarshal: %v\n%s", err, raw)
}
fmt.Fprint(w, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}))
defer server.Close()

client := NewClient("sk-test",
WithBaseURL(server.URL+"/v1"),
WithAPIMode("chat-completions"),
WithModelFallbacks([]string{"deepseek/deepseek-chat:free", "openrouter/auto"}),
)
_, err := client.CreateMessage(context.Background(), anthropic.CreateMessageRequest{
Model:     "deepseek/deepseek-v4-pro",
MaxTokens: 16,
Messages:  []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "hi"}}}},
})
if err != nil {
t.Fatalf("CreateMessage error: %v", err)
}
if captured["model"] != "deepseek/deepseek-v4-pro" {
t.Fatalf("model = %v", captured["model"])
}
models, ok := captured["models"].([]any)
if !ok {
t.Fatalf("models missing or wrong type: %#v", captured["models"])
}
want := []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-chat:free", "openrouter/auto"}
if len(models) != len(want) {
t.Fatalf("models = %v, want %v", models, want)
}
for i := range want {
if models[i] != want[i] {
t.Fatalf("models = %v, want %v", models, want)
}
}
}

func TestChatCompletionsOmitsModelsWithoutFallbacks(t *testing.T) {
var captured map[string]any
server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
raw, _ := io.ReadAll(r.Body)
_ = json.Unmarshal(raw, &captured)
fmt.Fprint(w, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}))
defer server.Close()

client := NewClient("sk-test", WithBaseURL(server.URL+"/v1"), WithAPIMode("chat-completions"))
_, err := client.CreateMessage(context.Background(), anthropic.CreateMessageRequest{
Model:     "deepseek/deepseek-v4-pro",
MaxTokens: 16,
Messages:  []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "hi"}}}},
})
if err != nil {
t.Fatalf("CreateMessage error: %v", err)
}
if _, ok := captured["models"]; ok {
t.Fatalf("models must be omitted without fallbacks: %#v", captured)
}
}
