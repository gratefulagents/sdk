package agent

import "strings"

const omittedToolImageMarker = "\n[screenshot omitted to save context]"

// pruneToolOutputImages keeps at most maxImages tool-output images, dropping
// images from the oldest tool outputs first. Removal happens in chunks of
// maxImages so the conversation prefix only changes every maxImages new
// images, which keeps provider prompt caches warm. maxImages <= 0 disables
// pruning. The input slice and its items are never mutated; a new slice is
// returned only when something was pruned.
func pruneToolOutputImages(items []RunItem, maxImages int) []RunItem {
	if maxImages <= 0 {
		return items
	}
	total := 0
	for _, item := range items {
		if item.ToolOutput != nil {
			total += len(item.ToolOutput.Images)
		}
	}
	remove := total - maxImages
	remove -= remove % maxImages
	if remove <= 0 {
		return items
	}
	pruned := make([]RunItem, len(items))
	copy(pruned, items)
	for i := range pruned {
		if remove <= 0 {
			break
		}
		out := pruned[i].ToolOutput
		if out == nil || len(out.Images) == 0 {
			continue
		}
		value := *out
		if len(out.Images) <= remove {
			remove -= len(out.Images)
			value.Images = nil
		} else {
			value.Images = append([]ImageAttachment(nil), out.Images[remove:]...)
			remove = 0
		}
		if !strings.HasSuffix(value.Content, omittedToolImageMarker) {
			value.Content += omittedToolImageMarker
		}
		pruned[i].ToolOutput = &value
	}
	return pruned
}
