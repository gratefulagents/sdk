package agent

// estimateModelCost returns a provider-aware USD estimate plus whether the
// value should be considered known for display purposes.
func estimateModelCost(model Model, usage Usage) (float64, bool) {
	if model == nil {
		return 0, false
	}
	type costEstimator interface {
		EstimateCost(Usage) (float64, bool)
	}
	if estimator, ok := model.(costEstimator); ok {
		return estimator.EstimateCost(usage)
	}
	return model.CalculateCost(usage), true
}

func estimateRunResultCost(result *RunResult, fallbackModel Model) (float64, bool) {
	if result == nil {
		return 0, false
	}
	if len(result.RawResponses) == 0 {
		return estimateModelCost(fallbackModel, result.Usage)
	}

	var costUSD float64
	costKnown := true
	var responsesUsage Usage
	for _, response := range result.RawResponses {
		costUSD += response.CostUSD
		if !response.CostKnown {
			costKnown = false
		}
		responsesUsage.Add(response.Usage)
	}
	// Compaction calls (provider-side or LLM-summary) add usage to
	// result.Usage without appending to RawResponses; estimate that residual
	// so the summed cost matches the reported usage.
	residual := Usage{
		Requests:          max(result.Usage.Requests-responsesUsage.Requests, 0),
		InputTokens:       max(result.Usage.InputTokens-responsesUsage.InputTokens, 0),
		OutputTokens:      max(result.Usage.OutputTokens-responsesUsage.OutputTokens, 0),
		CacheReadTokens:   max(result.Usage.CacheReadTokens-responsesUsage.CacheReadTokens, 0),
		CacheCreateTokens: max(result.Usage.CacheCreateTokens-responsesUsage.CacheCreateTokens, 0),
	}
	if residual.TotalTokens() > 0 || residual.CacheReadTokens > 0 || residual.CacheCreateTokens > 0 {
		residualCost, residualKnown := estimateModelCost(fallbackModel, residual)
		costUSD += residualCost
		if !residualKnown {
			costKnown = false
		}
	}
	return costUSD, costKnown
}
