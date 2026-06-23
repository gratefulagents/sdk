package openai

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/internal/anthropic"
)

// drainChatStream feeds an SSE body through the reader and reassembles the
// final Anthropic response, mirroring the production consumer.
func drainChatStream(t *testing.T, sse string) *anthropic.CreateMessageResponse {
	t.Helper()
	reader := newChatStreamReader(&http.Response{Body: io.NopCloser(strings.NewReader(sse))})
	assembler := anthropic.NewStreamAssembler()
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		assembler.Add(ev)
	}
	return assembler.Response()
}

func TestChatStreamReaderReasoningAndText(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-1","model":"claude-opus-4.8","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_text":"Let me think"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_text":" about it."}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_opaque":"SIG123","content":"Hello"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":" world"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	resp := drainChatStream(t, sse)

	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2: %+v", len(resp.Content), resp.Content)
	}
	thinking := resp.Content[0]
	if thinking.Type != "thinking" {
		t.Fatalf("block[0].Type = %q, want thinking", thinking.Type)
	}
	if thinking.Thinking != "Let me think about it." {
		t.Fatalf("thinking text = %q", thinking.Thinking)
	}
	if thinking.Signature != "SIG123" {
		t.Fatalf("thinking signature = %q, want SIG123 (reasoning_opaque round-trip)", thinking.Signature)
	}
	text := resp.Content[1]
	if text.Type != "text" || text.Text != "Hello world" {
		t.Fatalf("text block = %+v, want type=text text=%q", text, "Hello world")
	}
	if resp.StopReason != anthropic.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Fatalf("usage = in:%d out:%d, want in:10 out:5", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
}

// TestChatStreamReaderNarrationToolCallsQuirk covers the GitHub Copilot quirk
// where a plain narration turn finishes with finish_reason="tool_calls" but
// carries no tool calls. It must be treated as end_turn, not tool_use.
func TestChatStreamReaderNarrationToolCallsQuirk(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"claude-opus-4.8","choices":[{"index":0,"delta":{"content":"Working on it"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	resp := drainChatStream(t, sse)

	if resp.StopReason != anthropic.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn (narration tool_calls quirk)", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want single text block", resp.Content)
	}
}

func TestChatStreamReaderToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"claude-opus-4.8","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"foo","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	resp := drainChatStream(t, sse)

	if resp.StopReason != anthropic.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1: %+v", len(resp.Content), resp.Content)
	}
	tool := resp.Content[0]
	if tool.Type != "tool_use" || tool.ID != "call_1" || tool.Name != "foo" {
		t.Fatalf("tool block = %+v", tool)
	}
	if string(tool.Input) != `{"a":1}` {
		t.Fatalf("tool input = %q, want %q", string(tool.Input), `{"a":1}`)
	}
}

// TestChatStreamReaderMultipleToolCalls verifies that two streamed tool calls
// are reassembled as two distinct, correctly-attributed tool_use blocks (the
// assembler appends to the last builder, so blocks must be strictly sequential).
func TestChatStreamReaderMultipleToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"claude-opus-4.8","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"alpha","arguments":"{\"x\":1}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"beta","arguments":"{\"y\":2}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	resp := drainChatStream(t, sse)

	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2: %+v", len(resp.Content), resp.Content)
	}
	a, b := resp.Content[0], resp.Content[1]
	if a.Type != "tool_use" || a.ID != "call_a" || a.Name != "alpha" || string(a.Input) != `{"x":1}` {
		t.Fatalf("tool[0] = %+v, want call_a/alpha/{\"x\":1}", a)
	}
	if b.Type != "tool_use" || b.ID != "call_b" || b.Name != "beta" || string(b.Input) != `{"y":2}` {
		t.Fatalf("tool[1] = %+v, want call_b/beta/{\"y\":2}", b)
	}
	if resp.StopReason != anthropic.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want tool_use", resp.StopReason)
	}
}

// TestChatStreamReaderReasoningThenTool verifies a reasoning block followed by a
// tool call (no text) yields a thinking block with its signature plus the tool.
func TestChatStreamReaderReasoningThenTool(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"claude-opus-4.8","choices":[{"index":0,"delta":{"reasoning_text":"pick a tool"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_opaque":"SIGZ","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"foo","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	resp := drainChatStream(t, sse)

	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (thinking+tool): %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "pick a tool" || resp.Content[0].Signature != "SIGZ" {
		t.Fatalf("thinking block = %+v", resp.Content[0])
	}
	if resp.Content[1].Type != "tool_use" || resp.Content[1].Name != "foo" {
		t.Fatalf("tool block = %+v", resp.Content[1])
	}
	if resp.StopReason != anthropic.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want tool_use", resp.StopReason)
	}
}

func TestToAnthropicResponseFromChatReasoning(t *testing.T) {
	resp := chatCompletionResponse{
		ID:    "msg_1",
		Model: "claude-opus-4.8",
		Choices: []chatChoice{{
			Message: chatMessage{
				Role:            "assistant",
				Content:         "done",
				ReasoningText:   "because reasons",
				ReasoningOpaque: "OPAQUE1",
			},
			FinishReason: "stop",
		}},
	}
	out, err := toAnthropicResponseFromChat(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 2 {
		t.Fatalf("content = %d, want 2 (thinking+text): %+v", len(out.Content), out.Content)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "because reasons" {
		t.Fatalf("thinking block = %+v", out.Content[0])
	}
	if out.Content[0].Signature != "OPAQUE1" {
		t.Fatalf("signature = %q, want OPAQUE1", out.Content[0].Signature)
	}
	if out.StopReason != anthropic.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", out.StopReason)
	}
}

func TestToChatMessagesRoundTripsReasoning(t *testing.T) {
	req := anthropic.CreateMessageRequest{
		Messages: []anthropic.Message{{
			Role: anthropic.RoleAssistant,
			Content: []anthropic.ContentBlock{
				{Type: "thinking", Thinking: "deep thought", Signature: "SIGabc"},
				anthropic.NewTextBlock("answer"),
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
	if msgs[0].ReasoningOpaque != "SIGabc" {
		t.Fatalf("reasoning_opaque = %q, want SIGabc", msgs[0].ReasoningOpaque)
	}
	if msgs[0].ReasoningText != "deep thought" {
		t.Fatalf("reasoning_text = %q, want 'deep thought'", msgs[0].ReasoningText)
	}
}

// TestToChatMessagesOmitsReasoningWithoutSignature verifies reasoning_text is
// not resent without its encrypted signature (Copilot rejects that shape).
func TestToChatMessagesOmitsReasoningWithoutSignature(t *testing.T) {
	req := anthropic.CreateMessageRequest{
		Messages: []anthropic.Message{{
			Role: anthropic.RoleAssistant,
			Content: []anthropic.ContentBlock{
				{Type: "thinking", Thinking: "no signature here"},
				anthropic.NewTextBlock("answer"),
			},
		}},
	}
	msgs, err := toChatMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].ReasoningText != "" || msgs[0].ReasoningOpaque != "" {
		t.Fatalf("reasoning must be omitted without signature, got text=%q opaque=%q", msgs[0].ReasoningText, msgs[0].ReasoningOpaque)
	}
}
