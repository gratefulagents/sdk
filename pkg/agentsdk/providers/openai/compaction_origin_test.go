package openai

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// Compaction blobs stamped as Anthropic-origin are not decryptable by OpenAI
// and must be skipped instead of 400-ing the request after a provider switch.
func TestItemsToAnthropicMessagesSkipsAnthropicCompactionBlobs(t *testing.T) {
	items := []agentsdk.RunItem{
		{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "task"}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_anthropic", EncryptedContent: "anthropic-blob", CreatedBy: "anthropic",
		}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_openai", EncryptedContent: "openai-blob", CreatedBy: "openai",
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
		t.Fatalf("kept %d compaction blocks (%v), want 2 (openai + legacy unstamped)", len(kept), kept)
	}
	for _, enc := range kept {
		if enc == "anthropic-blob" {
			t.Fatalf("foreign Anthropic blob was forwarded to OpenAI: %v", kept)
		}
	}
}
