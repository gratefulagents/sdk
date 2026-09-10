package anthropic

import (
	"encoding/json"
	"testing"
)

func TestParallelToolResultImagesWireFormat(t *testing.T) {
	first := NewToolResultBlock("a", "inspect", false)
	first.ResultImages = []ImageSource{{Type: "base64", MediaType: "image/png", Data: "cG5n"}}
	params, _ := toSDKParams(&CreateMessageRequest{Model: "claude-sonnet-4-5", MaxTokens: 1024, Messages: []Message{
		{Role: RoleAssistant, Content: []ContentBlock{NewToolUseBlock("a", "AnalyzeImage", json.RawMessage(`{}`)), NewToolUseBlock("b", "read_file", json.RawMessage(`{}`))}},
		{Role: RoleUser, Content: []ContentBlock{first, NewToolResultBlock("b", "file text", false)}},
	}})
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Content []struct {
				Type    string `json:"type"`
				ID      string `json:"tool_use_id"`
				Content []struct {
					Type   string      `json:"type"`
					Source ImageSource `json:"source"`
				} `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	blocks := wire.Messages[1].Content
	if len(blocks) != 2 || blocks[0].Type != "tool_result" || blocks[1].Type != "tool_result" || blocks[0].ID != "a" || blocks[1].ID != "b" {
		t.Fatalf("wire = %s", encoded)
	}
	content := blocks[0].Content
	if len(content) != 2 || content[0].Type != "text" || content[1].Type != "image" || content[1].Source.Data != "cG5n" || content[1].Source.MediaType != "image/png" {
		t.Fatalf("wire = %s", encoded)
	}
}
