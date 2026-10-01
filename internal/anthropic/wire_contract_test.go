package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestToolSchemaAndCacheHTTPContract(t *testing.T) {
	const schema = `{"type":"object","title":"Lookup","description":"schema description","$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"id":{"type":"integer","minimum":9007199254740993}},"properties":{"id":{"$ref":"#/$defs/id"}},"required":["id"],"additionalProperties":false,"allOf":[{"minProperties":1}],"dependentRequired":{"id":["kind"]},"x-extension":{"enabled":true}}`
	for _, streaming := range []bool{false, true} {
		name := "blocking"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			bodies := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				bodies <- body
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_wire\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer srv.Close()
			cc := &CacheControl{Type: "ephemeral"}
			toolUse := NewToolUseBlock("call_1", "lookup", json.RawMessage(`{"id":1}`))
			toolUse.CacheControl = cc
			result := NewToolResultBlock("call_1", "found", false)
			result.CacheControl = cc
			req := CreateMessageRequest{
				Model: "claude-sonnet-4-5", MaxTokens: 100,
				System: []SystemBlock{{Type: "text", Text: "system", CacheControl: cc}},
				Tools: []ToolDefinition{
					{Name: "lookup", InputSchema: json.RawMessage(schema), CacheControl: cc},
					{Name: "empty", InputSchema: json.RawMessage(`{"type":"object"}`)},
					{Name: "absent"},
				},
				Messages: []Message{
					{Role: RoleUser, Content: []ContentBlock{NewTextBlock("hello")}},
					{Role: RoleAssistant, Content: []ContentBlock{toolUse}},
					{Role: RoleUser, Content: []ContentBlock{result}},
				},
			}
			client := NewClient("test", WithBaseURL(srv.URL))
			if streaming {
				stream, err := client.CreateMessageStream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				if _, err := stream.CollectResponse(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := client.CreateMessage(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			var body struct {
				Tools []struct {
					InputSchema  map[string]json.RawMessage `json:"input_schema"`
					CacheControl *CacheControl              `json:"cache_control"`
				} `json:"tools"`
				System   []SystemBlock `json:"system"`
				Messages []struct {
					Content []struct {
						CacheControl *CacheControl `json:"cache_control"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(<-bodies, &body); err != nil {
				t.Fatal(err)
			}
			var want map[string]json.RawMessage
			if err := json.Unmarshal([]byte(schema), &want); err != nil {
				t.Fatal(err)
			}
			if len(body.Tools) != 3 || !reflect.DeepEqual(body.Tools[0].InputSchema, want) {
				t.Fatalf("full schema not preserved: %+v", body.Tools)
			}
			for _, tool := range body.Tools[1:] {
				if string(tool.InputSchema["type"]) != `"object"` || string(tool.InputSchema["properties"]) != `{}` || tool.CacheControl != nil {
					t.Fatalf("parameterless tool changed: %+v", tool)
				}
			}
			for _, marker := range []*CacheControl{body.Tools[0].CacheControl, body.System[0].CacheControl, body.Messages[1].Content[0].CacheControl, body.Messages[2].Content[0].CacheControl} {
				if marker == nil || marker.Type != "ephemeral" {
					t.Fatalf("cache breakpoint lost: %+v", marker)
				}
			}
			if body.Messages[0].Content[0].CacheControl != nil {
				t.Fatal("cache marker added to unmarked block")
			}
		})
	}
}
