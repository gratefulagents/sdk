package anthropic

import (
	"regexp"
	"strings"
)

// ModelPricing holds per-million-token prices in USD.
type ModelPricing struct {
	InputPerMillion         float64
	OutputPerMillion        float64
	CacheReadPerMillion     float64
	CacheCreationPerMillion float64
}

// modelPricing maps model names to their pricing, verified against
// https://platform.claude.com/docs/en/about-claude/pricing (2026-09).
// CacheCreationPerMillion is the 5-minute cache-write rate (1.25x input).
var modelPricing = map[string]ModelPricing{
	// Fable 5.1 cache hits are priced at 0.025x input instead of the usual 0.1x.
	"claude-fable-5-1": {
		InputPerMillion:         10.0,
		OutputPerMillion:        50.0,
		CacheReadPerMillion:     0.25,
		CacheCreationPerMillion: 12.5,
	},
	"claude-fable-5": {
		InputPerMillion:         10.0,
		OutputPerMillion:        50.0,
		CacheReadPerMillion:     1.0,
		CacheCreationPerMillion: 12.5,
	},
	"claude-sonnet-4-6": {
		InputPerMillion:         3.0,
		OutputPerMillion:        15.0,
		CacheReadPerMillion:     0.30,
		CacheCreationPerMillion: 3.75,
	},
	"claude-opus-4-6": {
		InputPerMillion:         5.0,
		OutputPerMillion:        25.0,
		CacheReadPerMillion:     0.50,
		CacheCreationPerMillion: 6.25,
	},
	"claude-haiku-4-5": {
		InputPerMillion:         1.0,
		OutputPerMillion:        5.0,
		CacheReadPerMillion:     0.10,
		CacheCreationPerMillion: 1.25,
	},
}

// modelDateSuffix matches trailing release-date suffixes such as "-20251101".
var modelDateSuffix = regexp.MustCompile(`-\d{8}$`)

// pricingForModel resolves pricing for real-world model identifiers, which are
// commonly gateway-prefixed ("anthropic/claude-…") and date-suffixed
// ("claude-opus-4-6-20251101"). Gateways also spell versions with dots
// ("claude-fable-5.1"), which are normalized to the first-party hyphenated
// form. After normalization it falls back to matching the model family
// (fable/opus/sonnet/haiku) so e.g. an older opus release is billed at opus
// rather than sonnet rates.
func pricingForModel(model string) (ModelPricing, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if _, bare, ok := strings.Cut(model, "/"); ok && bare != "" {
		model = bare
	}
	if pricing, ok := modelPricing[model]; ok {
		return pricing, true
	}
	model = strings.ReplaceAll(model, ".", "-")
	if pricing, ok := modelPricing[model]; ok {
		return pricing, true
	}
	model = modelDateSuffix.ReplaceAllString(model, "")
	if pricing, ok := modelPricing[model]; ok {
		return pricing, true
	}
	for family, key := range map[string]string{
		"fable":  "claude-fable-5-1",
		"opus":   "claude-opus-4-6",
		"sonnet": "claude-sonnet-4-6",
		"haiku":  "claude-haiku-4-5",
	} {
		if strings.Contains(model, family) {
			return modelPricing[key], true
		}
	}
	return ModelPricing{}, false
}

// CalculateCost returns the cost in USD for a given model and usage.
func CalculateCost(model string, usage Usage) float64 {
	pricing, ok := pricingForModel(model)
	if !ok {
		// Fallback to sonnet pricing.
		pricing = modelPricing["claude-sonnet-4-6"]
	}

	cost := float64(usage.InputTokens) * pricing.InputPerMillion / 1_000_000
	cost += float64(usage.OutputTokens) * pricing.OutputPerMillion / 1_000_000
	cost += float64(usage.CacheReadInputTokens) * pricing.CacheReadPerMillion / 1_000_000
	cost += float64(usage.CacheCreationInputTokens) * pricing.CacheCreationPerMillion / 1_000_000

	return cost
}
