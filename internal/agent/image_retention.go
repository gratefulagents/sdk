package agent

import "strings"

// DefaultMaxRecentImages is how many of the most recent image attachments stay
// in model context; older images are replaced with a text placeholder.
const DefaultMaxRecentImages = 3

const (
	elidedImagePlaceholder    = "[image omitted from context to save space; re-open it with read_file if needed]"
	persistedImagePlaceholder = "[image omitted]"
)

// elideOldImages keeps only the keep most recent images across message and
// tool-output items, newest first, and appends a placeholder to the text of
// every item that lost images. It never mutates items or the structs they
// point to; when nothing is elided it returns items itself.
func elideOldImages(items []RunItem, keep int) []RunItem {
	return dropOldImages(items, keep, elidedImagePlaceholder)
}

// stripImagesForPersistence removes every image attachment so checkpoints,
// snapshots, and transcripts do not store base64 image payloads.
func stripImagesForPersistence(items []RunItem) []RunItem {
	return dropOldImages(items, 0, persistedImagePlaceholder)
}

// ElideOldImages exports elideOldImages for SDK hosts.
func ElideOldImages(items []RunItem, keep int) []RunItem { return elideOldImages(items, keep) }

// StripImagesForPersistence exports stripImagesForPersistence for SDK hosts.
func StripImagesForPersistence(items []RunItem) []RunItem { return stripImagesForPersistence(items) }

func dropOldImages(items []RunItem, keep int, placeholder string) []RunItem {
	if keep < 0 {
		keep = 0
	}
	var out []RunItem
	remaining := keep
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Message != nil && len(item.Message.Images) > 0 {
			kept := keptTail(len(item.Message.Images), &remaining)
			if kept < len(item.Message.Images) {
				if out == nil {
					out = append([]RunItem(nil), items...)
				}
				msg := *item.Message
				msg.Images = retainTail(msg.Images, kept)
				msg.Text = appendPlaceholder(msg.Text, placeholder)
				out[i].Message = &msg
			}
		}
		if item.ToolOutput != nil && len(item.ToolOutput.Images) > 0 {
			kept := keptTail(len(item.ToolOutput.Images), &remaining)
			if kept < len(item.ToolOutput.Images) {
				if out == nil {
					out = append([]RunItem(nil), items...)
				}
				output := *item.ToolOutput
				output.Images = retainTail(output.Images, kept)
				output.Content = appendPlaceholder(output.Content, placeholder)
				out[i].ToolOutput = &output
			}
		}
	}
	if out == nil {
		return items
	}
	return out
}

// keptTail returns how many of an item's n images fit in the remaining budget
// and consumes that much budget.
func keptTail(n int, remaining *int) int {
	kept := min(n, *remaining)
	*remaining -= kept
	return kept
}

// retainTail copies the newest kept images into a fresh slice so the caller's
// backing array is never shared or modified.
func retainTail(images []ImageAttachment, kept int) []ImageAttachment {
	if kept == 0 {
		return nil
	}
	return append([]ImageAttachment(nil), images[len(images)-kept:]...)
}

func appendPlaceholder(text, placeholder string) string {
	if strings.HasSuffix(text, placeholder) {
		return text
	}
	if text == "" {
		return placeholder
	}
	return text + "\n" + placeholder
}
