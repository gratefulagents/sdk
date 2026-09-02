package anthropic

import (
	"math"
	"testing"
)

func TestCalculateCost(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		usage    Usage
		wantCost float64
	}{
		{
			name:  "sonnet input only",
			model: "claude-sonnet-4-6",
			usage: Usage{InputTokens: 1_000_000},
			// 1M tokens * $3.00 / 1M = $3.00
			wantCost: 3.0,
		},
		{
			name:  "sonnet output only",
			model: "claude-sonnet-4-6",
			usage: Usage{OutputTokens: 1_000_000},
			// 1M tokens * $15.00 / 1M = $15.00
			wantCost: 15.0,
		},
		{
			name:  "sonnet mixed usage",
			model: "claude-sonnet-4-6",
			usage: Usage{
				InputTokens:              1000,
				OutputTokens:             500,
				CacheReadInputTokens:     2000,
				CacheCreationInputTokens: 300,
			},
			// 1000*3/1M + 500*15/1M + 2000*0.30/1M + 300*3.75/1M
			// = 0.003 + 0.0075 + 0.0006 + 0.001125 = 0.012225
			wantCost: 0.012225,
		},
		{
			name:  "opus pricing",
			model: "claude-opus-4-6",
			usage: Usage{InputTokens: 1000, OutputTokens: 1000},
			// 1000*5/1M + 1000*25/1M = 0.005 + 0.025 = 0.03
			wantCost: 0.03,
		},
		{
			name:  "haiku pricing",
			model: "claude-haiku-4-5",
			usage: Usage{InputTokens: 1000, OutputTokens: 1000},
			// 1000*1.0/1M + 1000*5.0/1M = 0.001 + 0.005 = 0.006
			wantCost: 0.006,
		},
		{
			name:     "unknown model falls back to sonnet",
			model:    "claude-unknown-99",
			usage:    Usage{InputTokens: 1_000_000},
			wantCost: 3.0,
		},
		{
			name:     "zero usage",
			model:    "claude-sonnet-4-6",
			usage:    Usage{},
			wantCost: 0.0,
		},
		{
			name:  "opus cache pricing",
			model: "claude-opus-4-6",
			usage: Usage{
				CacheReadInputTokens:     100_000,
				CacheCreationInputTokens: 10_000,
			},
			// 100000*0.50/1M + 10000*6.25/1M = 0.05 + 0.0625 = 0.1125
			wantCost: 0.1125,
		},
		{
			name:  "fable 5.1 pricing",
			model: "claude-fable-5-1",
			usage: Usage{
				InputTokens:              1000,
				OutputTokens:             1000,
				CacheReadInputTokens:     100_000,
				CacheCreationInputTokens: 10_000,
			},
			// 1000*10/1M + 1000*50/1M + 100000*0.25/1M + 10000*12.5/1M
			// = 0.01 + 0.05 + 0.025 + 0.125 = 0.21
			wantCost: 0.21,
		},
		{
			name:  "fable 5 cache reads cost 0.1x input, not 0.025x",
			model: "claude-fable-5",
			usage: Usage{
				InputTokens:              1000,
				OutputTokens:             1000,
				CacheReadInputTokens:     100_000,
				CacheCreationInputTokens: 10_000,
			},
			// 1000*10/1M + 1000*50/1M + 100000*1.0/1M + 10000*12.5/1M
			// = 0.01 + 0.05 + 0.1 + 0.125 = 0.285
			wantCost: 0.285,
		},
		{
			name:  "fable 5.1 gateway id with dotted version and prefix",
			model: "anthropic/claude-fable-5.1",
			usage: Usage{CacheReadInputTokens: 1_000_000},
			// dotted version must resolve to 5.1's $0.25 cache-read rate,
			// not the $1.00 fable-5 rate or the family fallback.
			wantCost: 0.25,
		},
		{
			name:     "fable 5.1 dated release",
			model:    "claude-fable-5-1-20260901",
			usage:    Usage{CacheReadInputTokens: 1_000_000},
			wantCost: 0.25,
		},
		{
			name:     "fable 5 dated release keeps fable 5 cache-read rate",
			model:    "claude-fable-5-20260601",
			usage:    Usage{CacheReadInputTokens: 1_000_000},
			wantCost: 1.0,
		},
		{
			name:     "unknown fable variant falls back to fable family, not sonnet",
			model:    "claude-fable-latest",
			usage:    Usage{InputTokens: 1_000_000},
			wantCost: 10.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.usage)
			if math.Abs(got-tt.wantCost) > 1e-9 {
				t.Errorf("CalculateCost(%s, %+v) = %f, want %f", tt.model, tt.usage, got, tt.wantCost)
			}
		})
	}
}
