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
			name:     "gpt-5.6-terra input",
			model:    "gpt-5.6-terra",
			usage:    anthropic.Usage{InputTokens: 1_000_000},
			wantCost: 2.5,
		},
		{
			name:     "gpt-6-astra input",
			model:    "gpt-6-astra",
			usage:    anthropic.Usage{InputTokens: 100_000},
			wantCost: 1.0,
		},
		{
			name:     "gpt-6-astra output",
			model:    "openai/gpt-6-astra",
			usage:    anthropic.Usage{OutputTokens: 100_000},
			wantCost: 5.0,
		},
		{
			name:     "gpt-6-astra long-context input above 272K uses doubled rates",
			model:    "gpt-6-astra",
			usage:    anthropic.Usage{InputTokens: 300_000, OutputTokens: 10_000},
			wantCost: 300_000*20.0/1_000_000 + 10_000*75.0/1_000_000,
		},
		{
			name:     "gpt-5.6-luna output",
			model:    "gpt-5.6-luna",
			usage:    anthropic.Usage{OutputTokens: 1_000_000},
			wantCost: 6.0,
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
			model:     "openai/gpt-5.6-terra",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  15.0,
			wantKnown: true,
		},
		{
			name:      "gpt-5.6 alias uses sol pricing",
			model:     "gpt-5.6",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  20.0,
			wantKnown: true,
		},
		{
			name:      "Daybreak Blue alias uses sol pricing",
			model:     "daybreak-blue-latest",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  20.0,
			wantKnown: true,
		},
		{
			name:      "prefixed Daybreak Blue alias uses sol pricing",
			model:     "openai/gpt-daybreak-blue-latest",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  20.0,
			wantKnown: true,
		},
		{
			name:      "Daybreak Red alias uses cyber pricing",
			model:     "daybreak-red-latest",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  75.0,
			wantKnown: true,
		},
		{
			name:      "gpt-prefixed Daybreak Red alias uses cyber pricing",
			model:     "gpt-daybreak-red-latest",
			usage:     anthropic.Usage{OutputTokens: 1_000_000},
			wantCost:  75.0,
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
			name:  "gpt-5.6-luna cached input uses cached price",
			model: "gpt-5.6-luna",
			usage: anthropic.Usage{
				InputTokens:          1_000_000,
				CacheReadInputTokens: 800_000,
			},
			wantCost:  0.28,
			wantKnown: true,
		},
		{
			name:  "gpt-5.6-luna applies cache-write premium",
			model: "gpt-5.6-luna",
			usage: anthropic.Usage{
				InputTokens:              1_000_000,
				CacheReadInputTokens:     200_000,
				CacheCreationInputTokens: 300_000,
			},
			wantCost:  0.895,
			wantKnown: true,
		},
		{
			name:  "gpt-5.6-sol uses short-context Daybreak pricing at threshold",
			model: "gpt-5.6-sol",
			usage: anthropic.Usage{
				InputTokens:              272_000,
				CacheReadInputTokens:     100_000,
				CacheCreationInputTokens: 100_000,
				OutputTokens:             1_000_000,
			},
			wantCost:  20.828,
			wantKnown: true,
		},
		{
			name:  "gpt-5.6-sol uses long-context Daybreak pricing above threshold",
			model: "gpt-5.6-sol",
			usage: anthropic.Usage{
				InputTokens:              1_000_000,
				CacheReadInputTokens:     200_000,
				CacheCreationInputTokens: 300_000,
				OutputTokens:             1_000_000,
			},
			wantCost:  37.16,
			wantKnown: true,
		},
		{
			name:  "gpt-5.6-cyber uses its only price tier above long-context threshold",
			model: "gpt-5.6-cyber",
			usage: anthropic.Usage{
				InputTokens:              1_000_000,
				CacheReadInputTokens:     200_000,
				CacheCreationInputTokens: 300_000,
				OutputTokens:             1_000_000,
			},
			wantCost:  86.1875,
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

func TestGPT6Pricing(t *testing.T) {
	for _, model := range []struct {
		id                         string
		input, read, write, output float64
	}{
		{"gpt-6-astra", 10, 1, 12.5, 50},
		{"gpt-6-sol", 2, .2, 2.5, 10},
		{"gpt-6-luna", .1, .01, .125, .5},
	} {
		for _, prefix := range []string{"", "openai/"} {
			for _, input := range []int64{100000, 272000, 272001} {
				usage := anthropic.Usage{InputTokens: input, CacheReadInputTokens: 20000, CacheCreationInputTokens: 10000, OutputTokens: 5000}
				inputMultiplier, outputMultiplier := 1.0, 1.0
				if input > 272000 {
					inputMultiplier, outputMultiplier = 2, 1.5
				}
				want := ((float64(input-30000)*model.input+20000*model.read+10000*model.write)*inputMultiplier + 5000*model.output*outputMultiplier) / 1e6
				got, known := EstimateCost(prefix+model.id, usage)
				if !known || math.Abs(got-want) > 1e-9 {
					t.Errorf("EstimateCost(%s, input=%d) = %g, %v; want %g, true", prefix+model.id, input, got, known, want)
				}
			}
		}
	}
}
