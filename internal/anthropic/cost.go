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
	// Fable 5.1 and Mythos 5.1 cache hits are priced at 0.025x input instead
	// of the usual 0.1x.
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
	"claude-mythos-5-1": {
		InputPerMillion:         10.0,
		OutputPerMillion:        50.0,
		CacheReadPerMillion:     0.25,
		CacheCreationPerMillion: 12.5,
	},
	"claude-mythos-5": {
		InputPerMillion:         10.0,
		OutputPerMillion:        50.0,
		CacheReadPerMillion:     1.0,
		CacheCreationPerMillion: 12.5,
	},
	// Sonnet 5 launched at introductory $2/$10 pricing, which Anthropic has
	// since made permanent (the scheduled 2026-09-01 increase was cancelled).
	"claude-sonnet-5": {
		InputPerMillion:         2.0,
		OutputPerMillion:        10.0,
		CacheReadPerMillion:     0.20,
		CacheCreationPerMillion: 2.50,
	},
	"claude-sonnet-4-6": {
		InputPerMillion:         3.0,
		OutputPerMillion:        15.0,
		CacheReadPerMillion:     0.30,
		CacheCreationPerMillion: 3.75,
	},
	// Opus 4.5 through Opus 5 all share the $5/$25 tier and resolve through
	// the opus family fallback below.
	"claude-opus-4-6": {
		InputPerMillion:         5.0,
		OutputPerMillion:        25.0,
		CacheReadPerMillion:     0.50,
		CacheCreationPerMillion: 6.25,
	},
	// Opus 4 and 4.1 (retired on the first-party API, still served on Bedrock
	// and Vertex) were priced at the older $15/$75 tier.
	"claude-opus-4-1": {
		InputPerMillion:         15.0,
		OutputPerMillion:        75.0,
		CacheReadPerMillion:     1.50,
		CacheCreationPerMillion: 18.75,
	},
	"claude-opus-4": {
		InputPerMillion:         15.0,
		OutputPerMillion:        75.0,
		CacheReadPerMillion:     1.50,
		CacheCreationPerMillion: 18.75,
	},
	"claude-haiku-4-5": {
		InputPerMillion:         1.0,
		OutputPerMillion:        5.0,
		CacheReadPerMillion:     0.10,
		CacheCreationPerMillion: 1.25,
	},
	"claude-haiku-3-5": {
		InputPerMillion:         0.80,
		OutputPerMillion:        4.0,
		CacheReadPerMillion:     0.08,
		CacheCreationPerMillion: 1.0,
	},
}

// modelDateSuffix matches trailing release-date suffixes such as "-20251101".
var modelDateSuffix = regexp.MustCompile(`-\d{8}$`)

// legacyModelOrder matches the pre-Claude-4 id layout that puts the version
// before the family ("claude-3-5-haiku"), so it can be rewritten to the
// current "claude-haiku-3-5" form.
var legacyModelOrder = regexp.MustCompile(`^claude-(\d+(?:-\d+)?)-(haiku|sonnet|opus)$`)

// pricingForModel resolves pricing for real-world model identifiers, which are
// commonly gateway-prefixed ("anthropic/claude-…") and date-suffixed
// ("claude-opus-4-6-20251101"). Gateways also spell versions with dots
// ("claude-fable-5.1"), which are normalized to the first-party hyphenated
// form, and legacy version-first ids ("claude-3-5-haiku") are reordered.
// After normalization it falls back to matching the model family
// (fable/mythos/opus/sonnet/haiku) so e.g. an unlisted opus release is
// billed at opus rather than sonnet rates.
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
	model = legacyModelOrder.ReplaceAllString(model, "claude-$2-$1")
	if pricing, ok := modelPricing[model]; ok {
		return pricing, true
	}
	for family, key := range map[string]string{
		"fable":  "claude-fable-5-1",
		"mythos": "claude-mythos-5-1",
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
