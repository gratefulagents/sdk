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
	for _, response := range result.RawResponses {
		costUSD += response.CostUSD
		if !response.CostKnown {
			costKnown = false
		}
	}
	return costUSD, costKnown
}
