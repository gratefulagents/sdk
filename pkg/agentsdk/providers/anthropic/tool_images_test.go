package anthropic

import (
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"testing"
)

func TestToolOutputImagesBecomeNativeImageBlocks(t *testing.T) {
	messages := itemsToAnthropicMessages([]agentsdk.RunItem{{
		Type:       agentsdk.RunItemToolOutput,
		ToolOutput: &agentsdk.ToolOutputData{CallID: "image-call", Content: "inspect", Images: []agentsdk.ImageAttachment{{MediaType: "image/png", Data: "cG5n", Detail: "high"}, {}}},
	}})
	if len(messages) != 1 || len(messages[0].Content) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
	result, image := messages[0].Content[0], messages[0].Content[1]
	if result.Type != "tool_result" || result.ToolUseID != "image-call" || result.Content != "inspect" {
		t.Fatalf("result = %+v", result)
	}
	if image.Type != "image" || image.Source == nil || image.Source.Data != "cG5n" || image.Source.MediaType != "image/png" || image.Detail != "high" {
		t.Fatalf("image = %+v", image)
	}
}
