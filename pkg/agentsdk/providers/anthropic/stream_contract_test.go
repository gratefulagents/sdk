package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// Exercise the public adapter as well as the HTTP reader: consumers must never
// receive a successful final response after a truncated or errored SSE body.
func TestPublicStreamCompletionContract(t *testing.T) {
	const partial = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	const terminal = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const overload = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	for _, tc := range []struct {
		name, body, text     string
		wantError, retryable bool
	}{
		{name: "empty", wantError: true},
		{name: "truncated", body: partial, text: "partial", wantError: true},
		{name: "complete", body: partial + terminal, text: "partial"},
		{name: "overload before output", body: overload, wantError: true, retryable: true},
		{name: "overload after output", body: partial + overload, text: "partial", wantError: true, retryable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			model := NewAnthropicModelWithClient(internalanthropic.NewClient("test", internalanthropic.WithBaseURL(srv.URL)))
			stream, err := model.StreamResponse(ctx, agentsdk.ModelRequest{Model: "claude-sonnet-4-5", Input: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hello"}}}})
			if err != nil {
				t.Fatalf("StreamResponse: %v", err)
			}
			var text strings.Builder
			var streamErr error
			for ev := range stream.Events {
				if ev.Type == agentsdk.ModelStreamDelta {
					text.WriteString(ev.Delta)
				}
				if ev.Type == agentsdk.ModelStreamError {
					streamErr = ev.Error
				}
			}
			if ctx.Err() != nil {
				t.Fatalf("stream did not finish: %v", ctx.Err())
			}
			if text.String() != tc.text {
				t.Fatalf("text = %q, want %q", text.String(), tc.text)
			}
			final := stream.Final()
			if tc.wantError {
				if streamErr == nil || final != nil {
					t.Fatalf("error=%v final=%+v; want error and no final response", streamErr, final)
				}
				if tc.retryable && !model.GetRetryAdvice(streamErr).ShouldRetry {
					t.Fatalf("transient SSE error not retryable: %v", streamErr)
				}
			} else {
				if streamErr != nil || final == nil {
					t.Fatalf("error=%v final=%+v; want successful response", streamErr, final)
				}
				if final.Usage.InputTokens != 2 || final.Usage.OutputTokens != 3 {
					t.Fatalf("usage=%+v", final.Usage)
				}
			}
		})
	}
}
