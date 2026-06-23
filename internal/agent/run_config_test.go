package agent

import "testing"

func TestCompactionDefaultsForModel(t *testing.T) {
	tests := []struct {
		model       string
		wantTrigger int
		wantTarget  int
	}{
		{"gpt-5.5", 360000, 200000},
		{"openai/gpt-5.5", 360000, 200000},
		{"gpt-5.4", 360000, 200000},
		{"openai/gpt-5.4", 360000, 200000},
		// Small/fast variants are capped conservatively (~128K-class) so the
		// static fallback never lets a smaller model blow past its real context
		// window — the gpt-5.3-codex-spark freeze. Provider /models metadata
		// overrides these when available.
		{"gpt-5.4-mini", 110000, 60000},
		{"openai/gpt-5.4-mini", 110000, 60000},
		{"gpt-5.4-nano", 110000, 60000},
		{"gpt-5.3-codex", 360000, 200000},
		{"gpt-5.3-codex-spark", 110000, 60000},
		{"gpt-5.2-codex", 360000, 200000},
		{"gpt-5.2", 360000, 200000},
		{"gpt-5.1", 360000, 200000},
		{"claude-opus-4-6", 180000, 100000},
		{"anthropic/claude-opus-4-6", 180000, 100000},
		{"claude-sonnet-4-6", 180000, 100000},
		{"claude-haiku-4-5", 180000, 100000},
		{"unknown-model", 180000, 100000},
		{"", 180000, 100000},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			trigger, target := CompactionDefaultsForModel(tt.model)
			if trigger != tt.wantTrigger || target != tt.wantTarget {
				t.Errorf("CompactionDefaultsForModel(%q) = (%d, %d), want (%d, %d)",
					tt.model, trigger, target, tt.wantTrigger, tt.wantTarget)
			}
		})
	}
}
