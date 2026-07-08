package openai

// Post-provider-compaction wire-shape regression (run chat-gf-all-aqvafl):
// the runner's post-compaction input — [user task msg, compaction blob,
// preserved recent tool exchanges incl. encrypted reasoning, carry-forward] —
// must serialize losslessly into the /responses input array. Guards against
// union-marshaling or item-filtering regressions in toResponseInputItems.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	anthropic "github.com/gratefulagents/sdk/internal/anthropic"
)

func TestPostCompactionInputSerializesLosslessly(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"type":"response.created","response":{"id":"resp_x","model":"gpt-5.5","usage":{"input_tokens":1}}}`,
			`{"type":"response.content_part.added","output_index":0,"part":{"type":"output_text"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"ok"}`,
			`{"type":"response.output_text.done","output_index":0}`,
			`{"type":"response.completed","response":{"id":"resp_x","model":"gpt-5.5","usage":{"input_tokens":2,"output_tokens":3},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	blob := strings.Repeat("gAAAAABqTjbtNHpESmuLhzgebwPRb8js4ITPVs2kgIvD60IA7PX1", 600) // ~31k chars

	msgs := []anthropic.Message{
		// provider compact output: user task msg, then blob
		{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.NewTextBlock("ensure that whenever an agentrun is deleted from any trigger it deletes all the information about the agent run from the db..")}},
		{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{func() anthropic.ContentBlock {
			b := anthropic.NewCompactionBlock("cmp_001", blob, "openai")
			return b
		}()}},
	}
	// 14 preserved recent items: 7 tool_use/tool_result pairs (incl. one reasoning)
	for i := 0; i < 7; i++ {
		callID := fmt.Sprintf("call_recent_%d", i)
		think := anthropic.NewThinkingBlock("")
		think.ID = fmt.Sprintf("rs_recent_%d", i)
		think.EncryptedContent = "enc_reasoning_" + strings.Repeat("x", 200)
		msgs = append(msgs,
			anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{think}},
			anthropic.Message{Role: anthropic.RoleAssistant, Content: []anthropic.ContentBlock{anthropic.NewToolUseBlock(callID, "read_file", json.RawMessage(`{"path":"internal/store/postgres/collaboration.go"}`))}},
			anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.NewToolResultBlock(callID, strings.Repeat("line of file content\n", 40), false)}},
		)
	}
	// carry-forward message
	msgs = append(msgs, anthropic.Message{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.NewTextBlock("[COMPACTION CARRY-FORWARD]\nLive AgentRun state: mode=chat step=awaiting-user\n## Durable Working State\nCurrent objective: ...")}})

	req := anthropic.CreateMessageRequest{
		Model:     "gpt-5.5",
		MaxTokens: 8192,
		System:    []anthropic.SystemBlock{{Type: "text", Text: "you are an agent"}},
		Messages:  msgs,
		Thinking:  &anthropic.ThinkingConfig{BudgetTokens: 8192},
		Tools: []anthropic.ToolDefinition{
			{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		},
		CompactionThreshold: 200000,
	}

	client := NewClient("sk-test", WithBaseURL(server.URL+"/v1"))
	if _, err := client.CreateMessage(context.Background(), req); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	input, _ := captured["input"].([]any)
	counts := map[string]int{}
	for _, it := range input {
		m := it.(map[string]any)
		ty, _ := m["type"].(string)
		if ty == "" {
			ty = fmt.Sprintf("role=%v(message)", m["role"])
		}
		counts[ty]++
	}

	// expectations from the runner's post-compaction currentInput
	want := map[string]int{
		"compaction":           1,
		"function_call":        7,
		"function_call_output": 7,
		"reasoning":            7,
	}
	for ty, n := range want {
		if counts[ty] != n {
			t.Errorf("wire %s = %d, want %d", ty, counts[ty], n)
		}
	}
	// 2 user text messages (task + carry-forward)
	if got := counts["message"] + counts["role=user(message)"] + counts["role=<nil>(message)"]; got < 2 {
		t.Errorf("wire text messages = %d, want >= 2 (task + carry-forward)", got)
	}
}
