package context_compaction_test

import (
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestContextCompactionExample(t *testing.T) {
	var items []agentsdk.RunItem
	for i := 0; i < 24; i++ {
		items = append(items, agentsdk.RunItem{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: strings.Repeat("important project context ", 20)},
		})
	}

	compacted, before, after, ok, reason := agentsdk.MaybeCompactRunItems(items, agentsdk.CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               100,
		TargetTokens:                60,
		PreserveRecentItems:         4,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          3,
	})
	if !ok {
		t.Fatalf("compaction did not run: reason=%s before=%d after=%d", reason, before, after)
	}
	if len(compacted) >= len(items) || after >= before {
		t.Fatalf("ineffective compaction: len %d -> %d tokens %d -> %d", len(items), len(compacted), before, after)
	}
	if summary := agentsdk.ExtractCompactionSummary(compacted); !strings.Contains(summary, "[COMPACTED HISTORY SUMMARY]") {
		t.Fatalf("summary missing: %q", summary)
	}

	trigger, target := agentsdk.CompactionDefaultsForModel("gpt-5.5")
	if trigger <= target {
		t.Fatalf("defaults should trigger above target: trigger=%d target=%d", trigger, target)
	}
}
