package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunnerPrunesRetainedToolImages(t *testing.T) {
	call := func(id string) *ModelResponse {
		return &ModelResponse{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: id, Name: "image", Input: json.RawMessage(`{}`)}}}}
	}
	model := &mockModel{responses: []*ModelResponse{
		call("c1"), call("c2"), call("c3"), call("c4"),
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
	}}
	tool := &imageResultTool{FunctionTool: FunctionTool{ToolName: "image", ReadOnly: true}}
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test", Tools: []Tool{tool}}, nil, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 5 {
		t.Fatalf("requests = %d", len(model.requests))
	}
	if n := len(imageIDs(model.requests[3].Input)); n != 3 {
		t.Fatalf("request 4 images = %d, want 3", n)
	}
	last := model.requests[4].Input
	if n := len(imageIDs(last)); n != 3 {
		t.Fatalf("request 5 images = %d, want 3", n)
	}
	var pruned int
	for _, item := range last {
		if item.ToolOutput != nil && strings.HasSuffix(item.ToolOutput.Content, elidedImagePlaceholder) {
			if len(item.ToolOutput.Images) != 0 {
				t.Fatalf("pruned output kept images: %+v", item.ToolOutput)
			}
			pruned++
		}
	}
	if pruned != 1 {
		t.Fatalf("pruned outputs = %d, want 1", pruned)
	}
}
