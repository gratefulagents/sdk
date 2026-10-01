package agent

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func testImage(id int) ImageAttachment {
	return ImageAttachment{MediaType: "image/png", Data: fmt.Sprintf("img-%d", id)}
}

func userImageItem(text string, ids ...int) RunItem {
	msg := &MessageOutput{Text: text}
	for _, id := range ids {
		msg.Images = append(msg.Images, testImage(id))
	}
	return RunItem{Type: RunItemMessage, Message: msg}
}

func toolImageItem(content string, ids ...int) RunItem {
	out := &ToolOutputData{CallID: "call-" + content, Content: content}
	for _, id := range ids {
		out.Images = append(out.Images, testImage(id))
	}
	return RunItem{Type: RunItemToolOutput, ToolOutput: out}
}

func imageIDs(items []RunItem) []string {
	var ids []string
	for _, item := range items {
		if item.Message != nil {
			for _, img := range item.Message.Images {
				ids = append(ids, img.Data)
			}
		}
		if item.ToolOutput != nil {
			for _, img := range item.ToolOutput.Images {
				ids = append(ids, img.Data)
			}
		}
	}
	return ids
}

// deepSnapshot copies items and every pointed-to struct so the original can be
// compared after a call that must not mutate it.
func deepSnapshot(items []RunItem) []RunItem {
	out := make([]RunItem, len(items))
	for i, item := range items {
		out[i] = item
		if item.Message != nil {
			msg := *item.Message
			msg.Images = append([]ImageAttachment(nil), msg.Images...)
			out[i].Message = &msg
		}
		if item.ToolOutput != nil {
			output := *item.ToolOutput
			output.Images = append([]ImageAttachment(nil), output.Images...)
			out[i].ToolOutput = &output
		}
	}
	return out
}

func sameSlice(a, b []RunItem) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func TestElideOldImagesKeepsNewestAcrossItems(t *testing.T) {
	items := []RunItem{
		userImageItem("look", 1),
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "a", Name: "read_file"}},
		toolImageItem("first", 2),
		{Type: RunItemMessage, Message: &MessageOutput{Text: "assistant reply"}},
		toolImageItem("second", 3, 4),
		toolImageItem("third", 5),
	}
	got := elideOldImages(items, 3)
	if want := []string{"img-3", "img-4", "img-5"}; !reflect.DeepEqual(imageIDs(got), want) {
		t.Fatalf("kept images = %v, want %v", imageIDs(got), want)
	}
	for _, idx := range []int{0, 2} {
		text := got[idx].ToolOutput
		var content string
		if text != nil {
			content = text.Content
		} else {
			content = got[idx].Message.Text
		}
		if !strings.HasSuffix(content, "\n"+elidedImagePlaceholder) {
			t.Fatalf("item %d text = %q, want placeholder", idx, content)
		}
	}
	if got[4].ToolOutput.Content != "second" || got[5].ToolOutput.Content != "third" {
		t.Fatalf("kept items gained placeholders: %q, %q", got[4].ToolOutput.Content, got[5].ToolOutput.Content)
	}
	if got[3].Message.Text != "assistant reply" || got[1].ToolCall != items[1].ToolCall {
		t.Fatal("items without images changed")
	}
}

func TestElideOldImagesPartiallyTrimsSingleItem(t *testing.T) {
	items := []RunItem{toolImageItem("shots", 1, 2, 3, 4)}
	got := elideOldImages(items, 2)
	if want := []string{"img-3", "img-4"}; !reflect.DeepEqual(imageIDs(got), want) {
		t.Fatalf("images = %v, want %v", imageIDs(got), want)
	}
	if got[0].ToolOutput.Content != "shots\n"+elidedImagePlaceholder {
		t.Fatalf("content = %q", got[0].ToolOutput.Content)
	}
}

func TestElideOldImagesDoesNotMutateInput(t *testing.T) {
	items := []RunItem{
		userImageItem("look", 1, 2),
		toolImageItem("first", 3),
		toolImageItem("", 4),
		toolImageItem("latest", 5),
	}
	before := deepSnapshot(items)
	messagePtr, outputPtr := items[0].Message, items[1].ToolOutput
	imagesBacking := &items[0].Message.Images[0]

	got := elideOldImages(items, 1)

	if !reflect.DeepEqual(items, before) {
		t.Fatalf("input mutated:\n got %+v\nwant %+v", items, before)
	}
	if items[0].Message != messagePtr || items[1].ToolOutput != outputPtr || &items[0].Message.Images[0] != imagesBacking {
		t.Fatal("input pointers replaced")
	}
	if sameSlice(got, items) {
		t.Fatal("result shares the input slice despite changes")
	}
	if got[0].Message == items[0].Message || got[1].ToolOutput == items[1].ToolOutput {
		t.Fatal("changed items share pointed-to structs with input")
	}
	if got[3].ToolOutput != items[3].ToolOutput {
		t.Fatal("unchanged item was copied unnecessarily")
	}
	if want := []string{"img-5"}; !reflect.DeepEqual(imageIDs(got), want) {
		t.Fatalf("images = %v, want %v", imageIDs(got), want)
	}
	if got[2].ToolOutput.Content != elidedImagePlaceholder {
		t.Fatalf("empty content placeholder = %q", got[2].ToolOutput.Content)
	}

	// Mutating the result must not leak into the input.
	got[0].Message.Text = "changed"
	if items[0].Message.Text != "look" {
		t.Fatal("result aliases input message")
	}
}

func TestElideOldImagesPartialTrimDoesNotShareBackingArray(t *testing.T) {
	items := []RunItem{toolImageItem("shots", 1, 2, 3)}
	got := elideOldImages(items, 2)
	got[0].ToolOutput.Images[0].Data = "changed"
	if items[0].ToolOutput.Images[1].Data != "img-2" {
		t.Fatal("result images alias input backing array")
	}
}

func TestElideOldImagesReturnsInputWhenNothingToElide(t *testing.T) {
	cases := map[string][]RunItem{
		"nil":       nil,
		"no images": {{Type: RunItemMessage, Message: &MessageOutput{Text: "hi"}}, {Type: RunItemToolOutput, ToolOutput: &ToolOutputData{Content: "ok"}}},
		"at limit":  {userImageItem("a", 1), toolImageItem("b", 2), toolImageItem("c", 3)},
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			got := elideOldImages(items, DefaultMaxRecentImages)
			if !sameSlice(got, items) || len(got) != len(items) {
				t.Fatalf("got new slice %+v", got)
			}
		})
	}
}

func TestElideOldImagesNegativeKeepDropsAll(t *testing.T) {
	got := elideOldImages([]RunItem{toolImageItem("a", 1)}, -1)
	if len(imageIDs(got)) != 0 {
		t.Fatalf("images = %v", imageIDs(got))
	}
}

func TestStripImagesForPersistence(t *testing.T) {
	items := []RunItem{
		userImageItem("look", 1),
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "a"}},
		toolImageItem("out", 2, 3),
		toolImageItem("text only"),
	}
	before := deepSnapshot(items)
	got := stripImagesForPersistence(items)
	if !reflect.DeepEqual(items, before) {
		t.Fatal("input mutated")
	}
	if ids := imageIDs(got); len(ids) != 0 {
		t.Fatalf("images = %v, want none", ids)
	}
	if got[0].Message.Text != "look\n"+persistedImagePlaceholder || got[2].ToolOutput.Content != "out\n"+persistedImagePlaceholder {
		t.Fatalf("texts = %q, %q", got[0].Message.Text, got[2].ToolOutput.Content)
	}
	if got[3].ToolOutput != items[3].ToolOutput || got[1].ToolCall != items[1].ToolCall {
		t.Fatal("image-free items were copied")
	}
	if got[2].ToolOutput.CallID != items[2].ToolOutput.CallID {
		t.Fatal("tool output metadata lost")
	}

	clean := []RunItem{toolImageItem("none")}
	if out := stripImagesForPersistence(clean); !sameSlice(out, clean) {
		t.Fatal("stripImagesForPersistence copied an image-free slice")
	}
}
