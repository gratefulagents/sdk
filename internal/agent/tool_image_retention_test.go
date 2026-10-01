package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func imageToolOutputs(perOutput ...int) []RunItem {
	var items []RunItem
	for i, n := range perOutput {
		out := &ToolOutputData{CallID: fmt.Sprintf("call-%d", i), Content: fmt.Sprintf("out-%d", i)}
		for j := 0; j < n; j++ {
			out.Images = append(out.Images, ImageAttachment{MediaType: "image/jpeg", Data: fmt.Sprintf("img-%d-%d", i, j)})
		}
		items = append(items,
			RunItem{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: out.CallID, Name: "computer_use"}},
			RunItem{Type: RunItemToolOutput, ToolOutput: out},
		)
	}
	return items
}

func retainedImages(items []RunItem) []string {
	var data []string
	for _, item := range items {
		if item.ToolOutput != nil {
			for _, img := range item.ToolOutput.Images {
				data = append(data, img.Data)
			}
		}
	}
	return data
}

func TestPruneToolOutputImagesUnlimited(t *testing.T) {
	items := imageToolOutputs(1, 1, 1, 1, 1)
	for _, max := range []int{0, -1} {
		got := pruneToolOutputImages(items, max)
		if len(retainedImages(got)) != 5 {
			t.Fatalf("max=%d pruned images: %v", max, retainedImages(got))
		}
	}
}

func TestPruneToolOutputImagesChunksOldestFirst(t *testing.T) {
	cases := []struct {
		name  string
		total int
		max   int
		want  int
	}{
		{"under limit", 3, 3, 3},
		{"partial chunk kept", 5, 3, 5},
		{"one chunk removed", 6, 3, 3},
		{"remainder kept", 8, 3, 5},
		{"two chunks removed", 9, 3, 3},
		{"max one", 4, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			per := make([]int, tc.total)
			for i := range per {
				per[i] = 1
			}
			got := retainedImages(pruneToolOutputImages(imageToolOutputs(per...), tc.max))
			if len(got) != tc.want {
				t.Fatalf("retained %d images, want %d: %v", len(got), tc.want, got)
			}
			if got[len(got)-1] != fmt.Sprintf("img-%d-0", tc.total-1) || got[0] != fmt.Sprintf("img-%d-0", tc.total-tc.want) {
				t.Fatalf("expected newest images retained, got %v", got)
			}
		})
	}
}

func TestPruneToolOutputImagesMultiImageOutputs(t *testing.T) {
	got := pruneToolOutputImages(imageToolOutputs(2, 2, 2), 2)
	if imgs := retainedImages(got); strings.Join(imgs, ",") != "img-2-0,img-2-1" {
		t.Fatalf("retained %v", imgs)
	}
}

func TestPruneToolOutputImagesMarkerAndNoMutation(t *testing.T) {
	items := imageToolOutputs(1, 1, 1, 1)
	got := pruneToolOutputImages(items, 2)

	for i, item := range items {
		if item.ToolOutput != nil && (len(item.ToolOutput.Images) != 1 || strings.Contains(item.ToolOutput.Content, omittedToolImageMarker)) {
			t.Fatalf("input item %d mutated: %+v", i, item.ToolOutput)
		}
	}
	if &got[0] == &items[0] {
		t.Fatal("pruned result shares backing array with input")
	}
	wantContent := []string{"out-0" + omittedToolImageMarker, "out-1" + omittedToolImageMarker, "out-2", "out-3"}
	var contents []string
	for _, item := range got {
		if item.ToolOutput != nil {
			contents = append(contents, item.ToolOutput.Content)
		}
	}
	if strings.Join(contents, "|") != strings.Join(wantContent, "|") {
		t.Fatalf("contents = %q", contents)
	}

	// Pruning again after new images arrive must not stack markers on
	// already-pruned outputs.
	got = append(got, imageToolOutputs(0, 0, 0, 0, 1, 1)[8:]...)
	again := pruneToolOutputImages(got, 2)
	for _, item := range again {
		if item.ToolOutput != nil && strings.Count(item.ToolOutput.Content, omittedToolImageMarker) > 1 {
			t.Fatalf("marker appended twice: %q", item.ToolOutput.Content)
		}
	}
	if imgs := retainedImages(again); len(imgs) != 2 {
		t.Fatalf("retained %v", imgs)
	}
	if !strings.HasSuffix(again[5].ToolOutput.Content, omittedToolImageMarker) || again[1].ToolOutput.Content != "out-0"+omittedToolImageMarker {
		t.Fatalf("unexpected contents after second prune: %q / %q", again[1].ToolOutput.Content, again[5].ToolOutput.Content)
	}
	if same := pruneToolOutputImages(again, 2); &same[0] != &again[0] {
		t.Fatal("no-op prune should return the input slice")
	}
}

func TestRunnerPrunesRetainedToolImages(t *testing.T) {
	call := func(id string) *ModelResponse {
		return &ModelResponse{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: id, Name: "image", Input: json.RawMessage(`{}`)}}}}
	}
	model := &mockModel{responses: []*ModelResponse{
		call("c1"), call("c2"), call("c3"), call("c4"),
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
	}}
	tool := &imageResultTool{FunctionTool: FunctionTool{ToolName: "image", ReadOnly: true}}
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "test", Tools: []Tool{tool}}, nil, RunConfig{MaxRetainedToolImages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 5 {
		t.Fatalf("requests = %d", len(model.requests))
	}
	if n := len(retainedImages(model.requests[3].Input)); n != 3 {
		t.Fatalf("request 4 images = %d, want 3 (partial chunk retained)", n)
	}
	last := model.requests[4].Input
	if n := len(retainedImages(last)); n != 2 {
		t.Fatalf("request 5 images = %d, want 2", n)
	}
	var pruned int
	for _, item := range last {
		if item.ToolOutput != nil && strings.HasSuffix(item.ToolOutput.Content, omittedToolImageMarker) {
			if len(item.ToolOutput.Images) != 0 {
				t.Fatalf("pruned output kept images: %+v", item.ToolOutput)
			}
			pruned++
		}
	}
	if pruned != 2 {
		t.Fatalf("pruned outputs = %d, want 2", pruned)
	}
}
