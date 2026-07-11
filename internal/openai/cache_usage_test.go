package openai

import (
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestResponsesCacheWriteTokensNonStreaming(t *testing.T) {
	var resp responses.Response
	if err := resp.UnmarshalJSON([]byte(`{"id":"resp_cache","model":"gpt-5.6","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":70,"cache_write_tokens":11}}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := toAnthropicResponseFromResponses(&resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage.CacheReadInputTokens != 70 || got.Usage.CacheCreationInputTokens != 11 {
		t.Fatalf("usage = %+v, want cache read=70 create=11", got.Usage)
	}
}

func TestResponsesCacheWriteTokensStreaming(t *testing.T) {
	reader := &ResponsesStreamReader{}
	var event responses.ResponseStreamEventUnion
	if err := event.UnmarshalJSON([]byte(`{"type":"response.completed","response":{"id":"resp_cache","model":"gpt-5.6","status":"completed","output":[],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":70,"cache_write_tokens":11}}}}`)); err != nil {
		t.Fatal(err)
	}
	reader.translateEvent(event)
	if len(reader.buf) == 0 || reader.buf[0].Usage == nil {
		t.Fatalf("events = %+v, want completion usage", reader.buf)
	}
	usage := reader.buf[0].Usage
	if usage.CacheReadInputTokens != 70 || usage.CacheCreationInputTokens != 11 {
		t.Fatalf("usage = %+v, want cache read=70 create=11", usage)
	}
}

func TestResponsesCacheWriteTokensCompaction(t *testing.T) {
	var resp responses.CompactedResponse
	if err := resp.UnmarshalJSON([]byte(`{"id":"resp_compact","created_at":1,"object":"response.compaction","output":[],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":70,"cache_write_tokens":11},"output_tokens_details":{"reasoning_tokens":0}}}`)); err != nil {
		t.Fatal(err)
	}
	got := compactedResponseToConversation(&resp)
	if got.Usage.CacheReadInputTokens != 70 || got.Usage.CacheCreationInputTokens != 11 {
		t.Fatalf("usage = %+v, want cache read=70 create=11", got.Usage)
	}
}
