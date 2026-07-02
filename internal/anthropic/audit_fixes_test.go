package anthropic

// Regression tests for the 2026-07 full logic audit fixes:
//   - streamed usage keeps input/cache tokens from message_start
//   - compaction blocks round-trip (assembler + outgoing params)
//   - OutputSchema is sent as output_format

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamAssemblerKeepsInputTokensWhenMessageDeltaOmitsThem(t *testing.T) {
	a := NewStreamAssembler()
	a.Add(StreamEvent{
		Type: EventMessageStart,
		Message: &CreateMessageResponse{
			ID: "msg_1",
			Usage: Usage{
				InputTokens:              1200,
				CacheReadInputTokens:     800,
				CacheCreationInputTokens: 50,
			},
		},
	})
	// Typical Anthropic message_delta: only cumulative output tokens.
	a.Add(StreamEvent{
		Type:  EventMessageDelta,
		Delta: &DeltaBlock{Type: "message_delta", StopReason: "end_turn"},
		Usage: &Usage{OutputTokens: 42},
	})

	resp := a.Response()
	if resp.Usage.InputTokens != 1200 {
		t.Fatalf("InputTokens = %d, want 1200 (zeroed by message_delta)", resp.Usage.InputTokens)
	}
	if resp.Usage.CacheReadInputTokens != 800 || resp.Usage.CacheCreationInputTokens != 50 {
		t.Fatalf("cache tokens = %d/%d, want 800/50", resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
	}
	if resp.Usage.OutputTokens != 42 {
		t.Fatalf("OutputTokens = %d, want 42", resp.Usage.OutputTokens)
	}
}

func TestStreamAssemblerAssemblesCompactionBlocks(t *testing.T) {
	a := NewStreamAssembler()
	a.Add(StreamEvent{Type: EventMessageStart, Message: &CreateMessageResponse{ID: "msg_1"}})
	a.Add(StreamEvent{
		Type:         EventContentBlockStart,
		Index:        0,
		ContentBlock: &ContentBlock{Type: "compaction"},
	})
	a.Add(StreamEvent{
		Type:  EventContentBlockDelta,
		Index: 0,
		Delta: &DeltaBlock{Type: "compaction_delta", Content: "summary part 1 "},
	})
	a.Add(StreamEvent{
		Type:  EventContentBlockDelta,
		Index: 0,
		Delta: &DeltaBlock{Type: "compaction_delta", Content: "and part 2", EncryptedContent: "opaque-blob"},
	})
	a.Add(StreamEvent{Type: EventContentBlockStop, Index: 0})

	resp := a.Response()
	if len(resp.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(resp.Content))
	}
	block := resp.Content[0]
	if block.Type != "compaction" {
		t.Fatalf("block type = %q, want compaction", block.Type)
	}
	if block.Content != "summary part 1 and part 2" {
		t.Fatalf("compaction content = %q", block.Content)
	}
	if block.EncryptedContent != "opaque-blob" {
		t.Fatalf("encrypted content = %q, want opaque-blob", block.EncryptedContent)
	}
}

func TestStreamAssemblerHandlesCompactionEncryptedContentDelta(t *testing.T) {
	// The OpenAI Responses shim emits compaction_encrypted_content deltas.
	a := NewStreamAssembler()
	a.Add(StreamEvent{
		Type:         EventContentBlockStart,
		Index:        0,
		ContentBlock: &ContentBlock{Type: "compaction", ID: "comp_1"},
	})
	a.Add(StreamEvent{
		Type:  EventContentBlockDelta,
		Index: 0,
		Delta: &DeltaBlock{Type: "compaction_encrypted_content", EncryptedContent: "opaque"},
	})
	a.Add(StreamEvent{Type: EventContentBlockStop, Index: 0})

	resp := a.Response()
	if len(resp.Content) != 1 || resp.Content[0].EncryptedContent != "opaque" {
		t.Fatalf("compaction encrypted content lost: %+v", resp.Content)
	}
}

func TestToSDKParamsSendsOutputFormatForOutputSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	req := &CreateMessageRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 100,
		Messages:  []Message{{Role: RoleUser, Content: []ContentBlock{NewTextBlock("hi")}}},
		OutputSchema: &OutputSchema{
			Name:   "final",
			Schema: schema,
			Strict: true,
		},
	}
	params, _ := toSDKParams(req)
	raw, err := params.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	format, ok := body["output_format"].(map[string]any)
	if !ok {
		t.Fatalf("output_format missing from request body: %s", raw)
	}
	if format["type"] != "json_schema" {
		t.Fatalf("output_format.type = %v, want json_schema", format["type"])
	}
	if format["schema"] == nil {
		t.Fatalf("output_format.schema missing: %s", raw)
	}
}

func TestToSDKContentBlockRoundTripsCompaction(t *testing.T) {
	block := NewCompactionBlock("comp_1", "opaque-blob", "assistant")
	block.Content = "summary"
	union := toSDKContentBlock(block)
	if union.OfCompaction == nil {
		t.Fatalf("compaction block converted to %+v, want OfCompaction (an empty text block is rejected by the API)", union)
	}
	raw, err := union.OfCompaction.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"encrypted_content":"opaque-blob"`) || !strings.Contains(string(raw), `"content":"summary"`) {
		t.Fatalf("compaction param JSON = %s", raw)
	}

	// Absent summary must serialize as null, not "" (empty string is rejected).
	empty := NewCompactionBlock("comp_2", "opaque-blob", "")
	union = toSDKContentBlock(empty)
	if union.OfCompaction == nil {
		t.Fatal("compaction without content should still convert to OfCompaction")
	}
	raw, err = union.OfCompaction.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"content":null`) {
		t.Fatalf("empty compaction content should be null, got %s", raw)
	}
}
