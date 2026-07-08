package openai

import (
	"strings"
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

// Anthropic compaction blocks carry a plaintext summary alongside the
// encrypted blob; after a switch to OpenAI that summary must survive as an
// assistant text message instead of vanishing with the skipped blob.
func TestItemsToAnthropicMessagesDownConvertsAnthropicBlobSummary(t *testing.T) {
	items := []agentsdk.RunItem{
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_anthropic", EncryptedContent: "anthropic-blob", CreatedBy: "anthropic",
			Content: "user asked for X; agent finished steps A and B",
		}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_anthropic_opaque", EncryptedContent: "anthropic-blob-2", CreatedBy: "anthropic",
		}},
	}
	msgs := itemsToAnthropicMessages(items)

	var texts []string
	for _, msg := range msgs {
		for _, block := range msg.Content {
			switch block.Type {
			case "compaction":
				t.Fatalf("foreign blob forwarded: %+v", block)
			case "text":
				texts = append(texts, block.Text)
			}
		}
	}
	if len(texts) != 1 {
		t.Fatalf("got %d text blocks (%v), want 1 down-converted summary (opaque blob has nothing to keep)", len(texts), texts)
	}
	if !strings.Contains(texts[0], "steps A and B") || !strings.Contains(texts[0], "[CONTEXT SUMMARY") {
		t.Fatalf("down-converted summary = %q", texts[0])
	}
}
