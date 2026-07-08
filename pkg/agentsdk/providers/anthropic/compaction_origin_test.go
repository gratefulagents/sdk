package anthropic

import (
	"strings"
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

// A skipped foreign blob is the only remnant of the history pruned behind it.
// When the producing provider supplied a plaintext summary, it must be
// down-converted to an assistant text message instead of silently severing
// the conversation after a provider switch.
func TestItemsToAnthropicMessagesDownConvertsForeignBlobSummary(t *testing.T) {
	items := []agentsdk.RunItem{
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_openai", EncryptedContent: "openai-blob", CreatedBy: "openai",
			Content: "user asked for X; agent finished steps A and B",
		}},
		{Type: agentsdk.RunItemCompaction, Compaction: &agentsdk.CompactionData{
			ID: "cmp_openai_opaque", EncryptedContent: "openai-blob-2", CreatedBy: "openai",
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
