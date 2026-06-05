package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/internal/anthropic"
)

func TestToChatRequestEmitsReasoningEffort(t *testing.T) {
	tests := []struct {
		name   string
		effort string
		want   string // exact JSON fragment expected, or "" when omitted
	}{
		{name: "none disables reasoning", effort: "none", want: `"reasoning":{"effort":"none"}`},
		{name: "low", effort: "low", want: `"reasoning":{"effort":"low"}`},
		{name: "high", effort: "high", want: `"reasoning":{"effort":"high"}`},
		{name: "empty omits field", effort: "", want: ""},
		{name: "whitespace omits field", effort: "   ", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := anthropic.CreateMessageRequest{
				Model:           "openrouter/some-reasoning-model",
				MaxTokens:       1024,
				Messages:        []anthropic.Message{},
				ReasoningEffort: tc.effort,
			}
			chatReq, err := toChatRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(chatReq)
			if err != nil {
				t.Fatal(err)
			}
			got := string(body)
			if tc.want == "" {
				if strings.Contains(got, "\"reasoning\"") {
					t.Fatalf("reasoning must be omitted for effort %q, got %s", tc.effort, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("expected %s in body, got %s", tc.want, got)
			}
		})
	}
}
