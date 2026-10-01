package agentsdk

import "github.com/gratefulagents/sdk/internal/agent"

// DefaultMaxRecentImages is how many of the most recent image attachments the
// runner keeps in model context.
const DefaultMaxRecentImages = agent.DefaultMaxRecentImages

// ElideOldImages returns items with only the keep most recent images (message
// and tool-output attachments, newest first); older images are replaced by a
// text placeholder. The input and the structs it points to are never mutated,
// and the input slice is returned as-is when nothing is elided.
func ElideOldImages(items []RunItem, keep int) []RunItem {
	return agent.ElideOldImages(items, keep)
}

// StripImagesForPersistence returns items with every image attachment replaced
// by an "[image omitted]" placeholder, for checkpoints, snapshots, and
// transcripts. The input is never mutated.
func StripImagesForPersistence(items []RunItem) []RunItem {
	return agent.StripImagesForPersistence(items)
}
