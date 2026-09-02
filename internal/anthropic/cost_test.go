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
		{
			name:  "sonnet 5 uses the permanent $2/$10 tier",
			model: "claude-sonnet-5",
			usage: Usage{
				InputTokens:              1_000_000,
				OutputTokens:             1_000_000,
				CacheReadInputTokens:     1_000_000,
				CacheCreationInputTokens: 1_000_000,
			},
			// 2 + 10 + 0.20 + 2.50
			wantCost: 14.70,
		},
		{
			name:     "dated sonnet 5 does not fall back to sonnet 4.6 rates",
			model:    "anthropic/claude-sonnet-5-20260815",
			usage:    Usage{InputTokens: 1_000_000},
			wantCost: 2.0,
		},
		{
			name:     "sonnet 4.5 still resolves via the sonnet 4.6 family rate",
			model:    "claude-sonnet-4-5-20250929",
			usage:    Usage{InputTokens: 1_000_000},
			wantCost: 3.0,
		},
		{
			name:  "mythos 5.1 matches fable 5.1 pricing",
			model: "claude-mythos-5-1",
			usage: Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			// 10 + 0.25
			wantCost: 10.25,
		},
		{
			name:  "mythos 5 keeps the 0.1x cache-read rate",
			model: "claude-mythos-5",
			usage: Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			// 10 + 1.0
			wantCost: 11.0,
		},
		{
			name:     "unknown mythos variant falls back to mythos family",
			model:    "claude-mythos-latest",
			usage:    Usage{OutputTokens: 1_000_000},
			wantCost: 50.0,
		},
		{
			name:     "opus 5 resolves via the opus $5/$25 family rate",
			model:    "claude-opus-5",
			usage:    Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			wantCost: 30.0,
		},
		{
			name:     "opus 4.1 uses the legacy $15/$75 tier",
			model:    "claude-opus-4-1-20250805",
			usage:    Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			wantCost: 90.0,
		},
		{
			name:  "opus 4 uses the legacy $15/$75 tier including cache rates",
			model: "claude-opus-4-20250514",
			usage: Usage{
				CacheReadInputTokens:     1_000_000,
				CacheCreationInputTokens: 1_000_000,
			},
			// 1.50 + 18.75
			wantCost: 20.25,
		},
		{
			name:     "haiku 3.5 legacy version-first id",
			model:    "claude-3-5-haiku-20241022",
			usage:    Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			wantCost: 4.80,
		},
		{
			name:     "haiku 3.5 canonical id",
			model:    "claude-haiku-3-5",
			usage:    Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			wantCost: 4.80,
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
