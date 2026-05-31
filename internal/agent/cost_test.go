package agent

import "testing"

func TestEstimateRunResultCostSumsResponseCosts(t *testing.T) {
	result := &RunResult{
		RawResponses: []ModelResponse{
			{CostUSD: 0.25, CostKnown: true},
			{CostUSD: 0.75, CostKnown: true},
		},
	}

	got, known := estimateRunResultCost(result, nil)

	if got != 1.0 || !known {
		t.Fatalf("estimateRunResultCost() = (%f, %t), want (1.0, true)", got, known)
	}
}

func TestEstimateRunResultCostPropagatesUnknownCost(t *testing.T) {
	result := &RunResult{
		RawResponses: []ModelResponse{
			{CostUSD: 0.25, CostKnown: true},
			{CostUSD: 0.00, CostKnown: false},
		},
	}

	got, known := estimateRunResultCost(result, nil)

	if got != 0.25 || known {
		t.Fatalf("estimateRunResultCost() = (%f, %t), want (0.25, false)", got, known)
	}
}
