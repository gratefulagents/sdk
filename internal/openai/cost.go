package openai

import (
	"strings"

	"github.com/gratefulagents/sdk/internal/anthropic"
)

// ModelPricing holds per-million-token prices in USD. LongContext, when set,
// applies to requests with more than 272K input tokens.
type ModelPricing struct {
	InputPerMillion           float64
	CachedInputPerMillion     float64
	CacheWriteInputPerMillion float64
	OutputPerMillion          float64
	LongContext               *ModelPricing
}

const longContextInputThreshold = 272_000

// modelPricing holds OpenAI standard-tier list prices in USD per 1M tokens,
// verified against https://developers.openai.com/api/docs/pricing (2026-08).
// CachedInputPerMillion is the prompt-cache read rate. CacheWriteInputPerMillion
// is the GPT-5.6+ cache-write rate (1.25x regular input). OutputPerMillion already
// covers reasoning tokens (OpenAI includes them in output_tokens). Batch, Flex,
// and Priority tiers are not modeled. Keep in sync when OpenAI updates pricing.
var modelPricing = map[string]ModelPricing{
	"gpt-4.1": {
		InputPerMillion:       2.0,
		CachedInputPerMillion: 0.50,
		OutputPerMillion:      8.0,
	},
	"gpt-4.1-mini": {
		InputPerMillion:       0.4,
		CachedInputPerMillion: 0.10,
		OutputPerMillion:      1.6,
	},
	"gpt-4.1-nano": {
		InputPerMillion:       0.1,
		CachedInputPerMillion: 0.025,
		OutputPerMillion:      0.4,
	},
	"gpt-4o": {
		InputPerMillion:       2.5,
		CachedInputPerMillion: 1.25,
		OutputPerMillion:      10.0,
	},
	"gpt-4o-mini": {
		InputPerMillion:       0.15,
		CachedInputPerMillion: 0.075,
		OutputPerMillion:      0.6,
	},
	"gpt-5.1": {
		InputPerMillion:       1.25,
		CachedInputPerMillion: 0.125,
		OutputPerMillion:      10.0,
	},
	"gpt-5.1-codex": {
		InputPerMillion:       1.25,
		CachedInputPerMillion: 0.125,
		OutputPerMillion:      10.0,
	},
	"gpt-5.1-codex-max": {
		InputPerMillion:       1.25,
		CachedInputPerMillion: 0.125,
		OutputPerMillion:      10.0,
	},
	"gpt-5.1-codex-mini": {
		InputPerMillion:       0.25,
		CachedInputPerMillion: 0.025,
		OutputPerMillion:      2.0,
	},
	"gpt-5.2": {
		InputPerMillion:       1.75,
		CachedInputPerMillion: 0.175,
		OutputPerMillion:      14.0,
	},
	"gpt-5.2-codex": {
		InputPerMillion:       1.75,
		CachedInputPerMillion: 0.175,
		OutputPerMillion:      14.0,
	},
	"gpt-5.3-codex": {
		InputPerMillion:       1.75,
		CachedInputPerMillion: 0.175,
		OutputPerMillion:      14.0,
	},
	"gpt-5.3-codex-spark": {
		InputPerMillion:  0.4,
		OutputPerMillion: 1.6,
	},
	"gpt-5.4": {
		InputPerMillion:       2.5,
		CachedInputPerMillion: 0.25,
		OutputPerMillion:      15.0,
	},
	"gpt-5.4-mini": {
		InputPerMillion:       0.75,
		CachedInputPerMillion: 0.075,
		OutputPerMillion:      4.50,
	},
	"gpt-5.4-nano": {
		InputPerMillion:       0.2,
		CachedInputPerMillion: 0.02,
		OutputPerMillion:      1.25,
	},
	"gpt-5.6-sol": {
		InputPerMillion:           4.0,
		CachedInputPerMillion:     0.40,
		CacheWriteInputPerMillion: 5.0,
		OutputPerMillion:          20.0,
		LongContext: &ModelPricing{
			InputPerMillion:           8.0,
			CachedInputPerMillion:     0.80,
			CacheWriteInputPerMillion: 10.0,
			OutputPerMillion:          30.0,
		},
	},
	"gpt-5.6-cyber": {
		InputPerMillion:           12.5,
		CachedInputPerMillion:     1.25,
		CacheWriteInputPerMillion: 15.625,
		OutputPerMillion:          75.0,
	},
	"gpt-5.6-terra": {
		InputPerMillion:           2.5,
		CachedInputPerMillion:     0.25,
		CacheWriteInputPerMillion: 3.125,
		OutputPerMillion:          15.0,
	},
	"gpt-5.6-luna": {
		InputPerMillion:           1.0,
		CachedInputPerMillion:     0.10,
		CacheWriteInputPerMillion: 1.25,
		OutputPerMillion:          6.0,
	},
	"gpt-5.5": {
		InputPerMillion:       5.0,
		CachedInputPerMillion: 0.50,
		OutputPerMillion:      30.0,
	},
	"gpt-5-mini": {
		InputPerMillion:       0.25,
		CachedInputPerMillion: 0.025,
		OutputPerMillion:      2.0,
	},
	"gpt-5": {
		InputPerMillion:       1.25,
		CachedInputPerMillion: 0.125,
		OutputPerMillion:      10.0,
	},
	"gpt-5-codex": {
		InputPerMillion:       1.25,
		CachedInputPerMillion: 0.125,
		OutputPerMillion:      10.0,
	},
	"gpt-5-nano": {
		InputPerMillion:       0.05,
		CachedInputPerMillion: 0.005,
		OutputPerMillion:      0.4,
	},
}

func normalizePricingModel(model string) string {
	model = strings.TrimSpace(model)
	if prefix, bare, ok := strings.Cut(model, "/"); ok && strings.TrimSpace(prefix) == "openai" && strings.TrimSpace(bare) != "" {
		model = strings.TrimSpace(bare)
	}
	if model == "gpt-4" {
		return "gpt-4.1"
	}
	if model == "gpt-5.6" || model == "daybreak-blue-latest" || model == "gpt-daybreak-blue-latest" {
		return "gpt-5.6-sol"
	}
	if model == "daybreak-red-latest" || model == "gpt-daybreak-red-latest" {
		return "gpt-5.6-cyber"
	}
	return model
}

// EstimateCost returns the estimated USD cost for an OpenAI-compatible request
// together with whether pricing for the model is known.
func EstimateCost(model string, usage anthropic.Usage) (float64, bool) {
	pricing, ok := modelPricing[normalizePricingModel(model)]
	if !ok {
		return 0, false
	}

	inputTokens := max(usage.InputTokens, 0)
	if pricing.LongContext != nil && inputTokens > longContextInputThreshold {
		pricing = *pricing.LongContext
	}
	cacheReadTokens := max(usage.CacheReadInputTokens, 0)
	if cacheReadTokens > inputTokens {
		cacheReadTokens = inputTokens
	}
	cacheWriteTokens := max(usage.CacheCreationInputTokens, 0)
	if cacheWriteTokens > inputTokens-cacheReadTokens {
		cacheWriteTokens = inputTokens - cacheReadTokens
	}
	uncachedInputTokens := inputTokens
	if pricing.CachedInputPerMillion > 0 {
		uncachedInputTokens -= cacheReadTokens
	}
	if pricing.CacheWriteInputPerMillion > 0 {
		uncachedInputTokens -= cacheWriteTokens
	}

	cost := float64(uncachedInputTokens) * pricing.InputPerMillion / 1_000_000
	if pricing.CachedInputPerMillion > 0 {
		cost += float64(cacheReadTokens) * pricing.CachedInputPerMillion / 1_000_000
	}
	if pricing.CacheWriteInputPerMillion > 0 {
		cost += float64(cacheWriteTokens) * pricing.CacheWriteInputPerMillion / 1_000_000
	}
	cost += float64(usage.OutputTokens) * pricing.OutputPerMillion / 1_000_000
	return cost, true
}

// CalculateCost returns the estimated USD cost for an OpenAI-compatible request.
func CalculateCost(model string, usage anthropic.Usage) float64 {
	cost, _ := EstimateCost(model, usage)
	return cost
}
