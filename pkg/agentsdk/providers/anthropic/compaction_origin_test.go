package anthropic

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// Encrypted compaction blobs from a foreign provider (e.g. OpenAI) are not
// decryptable by Anthropic and would 400 the whole request after a
// cross-provider fallback/model switch; they must be skipped.
func TestItemsToAnthropicMessagesSkipsForeignCompactionBlobs(t *testing.T) {
	items := []agentsdk.RunItem{
		{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "task"}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_openai", EncryptedContent: "openai-blob", CreatedBy: "openai",
		}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_native", EncryptedContent: "anthropic-blob", CreatedBy: "anthropic",
		}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_legacy", EncryptedContent: "unstamped-blob",
		}},
	}
	msgs := itemsToAnthropicMessages(items)

	var kept []string
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if block.Type == "compaction" {
				kept = append(kept, block.EncryptedContent)
			}
		}
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d compaction blocks (%v), want 2 (native + legacy unstamped)", len(kept), kept)
	}
	for _, enc := range kept {
		if enc == "openai-blob" {
			t.Fatalf("foreign OpenAI blob was forwarded to Anthropic: %v", kept)
		}
	}
}
