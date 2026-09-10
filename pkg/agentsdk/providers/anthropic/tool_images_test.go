package anthropic

import (
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"testing"
)

func TestToolOutputImagesBecomeNativeImageBlocks(t *testing.T) {
	messages := itemsToAnthropicMessages([]agentsdk.RunItem{{
		Type:       agentsdk.RunItemToolOutput,
		ToolOutput: &agentsdk.ToolOutputData{CallID: "image-call", Content: "inspect", Images: []agentsdk.ImageAttachment{{MediaType: "image/png", Data: "cG5n", Detail: "high"}, {}}},
	}, {
		Type:       agentsdk.RunItemToolOutput,
		ToolOutput: &agentsdk.ToolOutputData{CallID: "second-call", Content: "file text"},
	}})
	if len(messages) != 2 || len(messages[0].Content) != 1 {
		t.Fatalf("messages = %+v", messages)
	}
	result := messages[0].Content[0]
	if result.Type != "tool_result" || result.ToolUseID != "image-call" || result.Content != "inspect" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.ResultImages) != 1 || result.ResultImages[0].Data != "cG5n" || result.ResultImages[0].MediaType != "image/png" {
		t.Fatalf("images = %+v", result.ResultImages)
	}
}
