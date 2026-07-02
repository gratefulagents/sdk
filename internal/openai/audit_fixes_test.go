package openai

// Regression tests for the 2026-07 full logic audit fixes:
//   - refusals surface as text instead of an empty (retried) response
//   - images are forwarded on the chat-completions path
//   - truncation (finish_reason=length / max_output_tokens) is not masked by tool_use

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/internal/anthropic"
)

func TestChatResponseSurfacesRefusal(t *testing.T) {
	resp := chatCompletionResponse{
		ID:    "chatcmpl-1",
		Model: "gpt-test",
		Choices: []chatChoice{{
			Message: chatMessage{
				Role:    "assistant",
				Refusal: "I can't help with that.",
			},
			FinishReason: "stop",
		}},
	}
	out, err := toAnthropicResponseFromChat(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) == 0 {
		t.Fatal("refusal produced empty content; would be retried as ResponseFailedError")
	}
	if !strings.Contains(out.Content[0].Text, "I can't help with that.") {
		t.Fatalf("refusal text lost: %+v", out.Content)
	}
}

func TestChatStopReasonPrefersTruncationOverToolCalls(t *testing.T) {
	if got := chatStopReason("length", true); got != anthropic.StopReasonMaxTokens {
		t.Fatalf("chatStopReason(length, hasToolCalls) = %q, want max_tokens", got)
	}
	if got := chatStopReason("tool_calls", true); got != anthropic.StopReasonToolUse {
		t.Fatalf("chatStopReason(tool_calls, hasToolCalls) = %q, want tool_use", got)
	}
	// Copilot narration turns: finish_reason=tool_calls without tool calls.
	if got := chatStopReason("tool_calls", false); got != anthropic.StopReasonEndTurn {
		t.Fatalf("chatStopReason(tool_calls, none) = %q, want end_turn", got)
	}
}

func TestToChatMessagesForwardsImages(t *testing.T) {
	req := anthropic.CreateMessageRequest{
		Model: "gpt-test",
		Messages: []anthropic.Message{{
			Role: anthropic.RoleUser,
			Content: []anthropic.ContentBlock{
				anthropic.NewTextBlock("what is in this image?"),
				anthropic.NewImageBlock("image/png", "aGVsbG8="),
			},
		}},
	}
	msgs, err := toChatMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	parts, ok := msgs[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("user message with image should be multipart, got %T (image silently dropped)", msgs[0].Content)
	}
	var sawText, sawImage bool
	for _, part := range parts {
		switch part["type"] {
		case "text":
			sawText = true
		case "image_url":
			img, _ := part["image_url"].(map[string]any)
			url, _ := img["url"].(string)
			if !strings.HasPrefix(url, "data:image/png;base64,aGVsbG8=") {
				t.Fatalf("image url = %q", url)
			}
			sawImage = true
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("multipart content missing text/image: %+v", parts)
	}
	// Round-trip through JSON to ensure the wire shape is valid.
	if _, err := json.Marshal(msgs); err != nil {
		t.Fatal(err)
	}
}

func TestToChatMessagesTextOnlyStaysString(t *testing.T) {
	req := anthropic.CreateMessageRequest{
		Model: "gpt-test",
		Messages: []anthropic.Message{{
			Role:    anthropic.RoleUser,
			Content: []anthropic.ContentBlock{anthropic.NewTextBlock("hello")},
		}},
	}
	msgs, err := toChatMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if _, ok := msgs[0].Content.(string); !ok {
		t.Fatalf("text-only user content should remain a plain string, got %T", msgs[0].Content)
	}
}

func TestChatStreamSurfacesRefusalDeltas(t *testing.T) {
	r := &chatStreamReader{toolAccs: map[int]*chatStreamToolAcc{}}
	r.translateChunk(chatCompletionChunk{
		ID:    "chatcmpl-1",
		Model: "gpt-test",
		Choices: []chatChunkChoice{{
			Delta: chatChunkDelta{Refusal: "I can't "},
		}},
	})
	r.translateChunk(chatCompletionChunk{
		Choices: []chatChunkChoice{{
			Delta:        chatChunkDelta{Refusal: "help with that."},
			FinishReason: "stop",
		}},
	})
	r.finalize()

	assembler := anthropic.NewStreamAssembler()
	for _, ev := range r.buf {
		assembler.Add(ev)
	}
	resp := assembler.Response()
	var text string
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	if !strings.Contains(text, "I can't help with that.") {
		t.Fatalf("streamed refusal lost, content=%+v", resp.Content)
	}
}
