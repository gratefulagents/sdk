package agent

import (
	"context"
	"encoding/json"
	"testing"
)

type imageResultTool struct{ FunctionTool }

func (*imageResultTool) Execute(context.Context, json.RawMessage, string) (ToolResult, error) {
	return ToolResult{Content: "inspect", Images: []ImageAttachment{{MediaType: "image/png", Data: "cG5n", Detail: "high"}}}, nil
}
func TestRunnerPreservesToolImagesInNextRequest(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "image-call", Name: "image", Input: json.RawMessage(`{}`)}}}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "seen"}}}},
	}}
	tool := &imageResultTool{FunctionTool: FunctionTool{ToolName: "image", ReadOnly: true}}
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test", Tools: []Tool{tool}}, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests = %d", len(model.requests))
	}
	for _, item := range model.requests[1].Input {
		if item.ToolOutput == nil {
			continue
		}
		encoded, err := json.Marshal(item.ToolOutput)
		if err != nil {
			t.Fatal(err)
		}
		var restored ToolOutputData
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		images := restored.Images
		if len(images) != 1 || images[0].Data != "cG5n" || images[0].Detail != "high" {
			t.Fatalf("images = %+v", images)
		}
		return
	}
	t.Fatal("tool output missing from next request")
}
