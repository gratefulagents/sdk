package anthropic

// ModelPricing holds per-million-token prices in USD.
type ModelPricing struct {
	InputPerMillion         float64
	OutputPerMillion        float64
	CacheReadPerMillion     float64
	CacheCreationPerMillion float64
}

// modelPricing maps model names to their pricing.
var modelPricing = map[string]ModelPricing{
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

// CalculateCost returns the cost in USD for a given model and usage.
func CalculateCost(model string, usage Usage) float64 {
	pricing, ok := modelPricing[model]
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
