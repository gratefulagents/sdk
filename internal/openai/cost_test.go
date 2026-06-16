package openai

import (
	"math"
	"testing"

	"github.com/gratefulagents/sdk/internal/anthropic"
)

func TestCalculateCost(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		usage    anthropic.Usage
		wantCost float64
	}{
		{
			name:     "gpt-4.1 input",
			model:    "gpt-4.1",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 2.0,
		},
		{
			name:     "gpt-4.1 output",
			model:    "gpt-4.1",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 8.0,
		},
		{
			name:  "gpt-4.1-mini mixed",
			model: "gpt-4.1-mini",
			usage: anthropic.Usage{
				InputTokens:  2000,
				OutputTokens: 1000,
			},
			wantCost: 0.0024,
		},
		{
			name:     "gpt-5.3-codex input uses corrected rate",
			model:    "gpt-5.3-codex",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 1.75,
		},
		{
			name:     "gpt-5.3-codex output uses corrected rate",
			model:    "gpt-5.3-codex",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 14.0,
		},
		{
			name:     "gpt-5.4-nano input uses corrected rate",
			model:    "gpt-5.4-nano",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 0.2,
		},
		{
			name:     "gpt-5.4-nano output uses corrected rate",
			model:    "gpt-5.4-nano",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 1.25,
		},
		{
			name:     "gpt-5 output",
			model:    "gpt-5",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 10.0,
		},
		{
			name:     "gpt-5-nano output",
			model:    "gpt-5-nano",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 0.4,
		},
		{
			name:     "gpt-5.1-codex-mini output",
			model:    "gpt-5.1-codex-mini",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 2.0,
		},
		{
			name:     "gpt-4.1-nano output",
			model:    "gpt-4.1-nano",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 0.4,
		},
		{
			name:     "gpt-5.3-codex-spark input",
			model:    "gpt-5.3-codex-spark",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 0.4,
		},
		{
			name:     "gpt-5.4 input",
			model:    "gpt-5.4",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 2.5,
		},
		{
			name:     "gpt-5.4-mini input",
			model:    "gpt-5.4-mini",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 0.75,
		},
		{
			name:     "gpt-5.5 output",
			model:    "gpt-5.5",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 30.0,
		},
		{
			name:     "unknown model has zero cost",
			model:    "custom-model",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.model, tt.usage)
			if math.Abs(got-tt.wantCost) > 1e-9 {
				t.Fatalf("CalculateCost(%q, %+v) = %f, want %f", tt.model, tt.usage, got, tt.wantCost)
			}
		})
	}
}

func TestEstimateCost(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		usage     anthropic.Usage
		wantCost  float64
		wantKnown bool
	}{
		{
			name:      "gpt-4 alias uses gpt-4.1 pricing",
			model:     "gpt-4",
			usage:     anthropic.Usage{InputTokens: 1_000_000},
			wantCost:  2.0,
			wantKnown: true,
		},
		{
			name:      "whitespace trimmed",
			model:     " gpt-4 ",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  8.0,
			wantKnown: true,
		},
		{
			name:      "openai provider prefix stripped",
			model:     "openai/gpt-5.5",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  30.0,
			wantKnown: true,
		},
		{
			name:      "unknown model is not known",
			model:     "custom-model",
			usage:     anthropic.Usage{InputTokens: 1_000_000},
			wantCost:  0,
			wantKnown: false,
		},
		{
			name:  "cached input uses cached price",
			model: "gpt-5.5",
			usage: anthropic.Usage{
				InputTokens:          1_000_000,
				CacheReadInputTokens: 800_000,
			},
			wantCost:  1.4,
			wantKnown: true,
		},
		{
			name:  "gpt-5.3-codex applies cached discount",
			model: "gpt-5.3-codex",
			usage: anthropic.Usage{
				InputTokens:          1_000_000,
				CacheReadInputTokens: 800_000,
			},
			wantCost:  0.49,
			wantKnown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCost, gotKnown := EstimateCost(tt.model, tt.usage)
			if math.Abs(gotCost-tt.wantCost) > 1e-9 || gotKnown != tt.wantKnown {
				t.Fatalf("EstimateCost(%q, %+v) = (%f, %t), want (%f, %t)", tt.model, tt.usage, gotCost, gotKnown, tt.wantCost, tt.wantKnown)
			}
		})
	}
}
