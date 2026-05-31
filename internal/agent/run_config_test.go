package agent

import "testing"

func TestCompactionDefaultsForModel(t *testing.T) {
	tests := []struct {
		model       string
		wantTrigger int
		wantTarget  int
	}{
		{"gpt-5.5", 900000, 500000},
		{"openai/gpt-5.5", 900000, 500000},
		{"gpt-5.4", 900000, 500000},
		{"openai/gpt-5.4", 900000, 500000},
		{"gpt-5.4-mini", 115000, 64000},
		{"openai/gpt-5.4-mini", 115000, 64000},
		{"gpt-5.3-codex", 900000, 500000},
		{"gpt-5.3-codex-spark", 900000, 500000},
		{"gpt-5.2-codex", 900000, 500000},
		{"gpt-5.2", 900000, 500000},
		{"gpt-5.1", 900000, 500000},
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
