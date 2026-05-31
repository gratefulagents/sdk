package mode

import (
	"strings"
	"testing"
)

func TestPhaseEntryGatePlanExists(t *testing.T) {
	gates := []PhaseGate{{Require: "plan_exists", Message: "Save a plan first"}}
	if result := EvaluatePhaseEntryGates(gates, EvidenceContext{PlanExists: true}); result != nil {
		t.Fatalf("EvaluatePhaseEntryGates() = %#v, want nil", result)
	}
	result := EvaluatePhaseEntryGates(gates, EvidenceContext{PlanExists: false})
	if result == nil || result.Message != "Save a plan first" {
		t.Fatalf("failure = %#v, want custom message", result)
	}
}

func TestPhaseEntryGateDefaultMessage(t *testing.T) {
	result := EvaluatePhaseEntryGates([]PhaseGate{{Require: "plan_exists"}}, EvidenceContext{})
	if result == nil || !strings.Contains(result.Message, "no plan artifact") {
		t.Fatalf("failure = %#v, want default plan message", result)
	}
}

func TestPhaseEntryGateGitClean(t *testing.T) {
	result := EvaluatePhaseEntryGatesWithGitRunner(
		[]PhaseGate{{Require: "git_clean"}},
		EvidenceContext{WorkDir: "/fake"},
		func(workDir string, args ...string) ([]byte, error) { return []byte(""), nil },
	)
	if result != nil {
		t.Fatalf("EvaluatePhaseEntryGatesWithGitRunner() = %#v, want nil", result)
	}
}

func TestPhaseEntryGateUnknownCondition(t *testing.T) {
	result := EvaluatePhaseEntryGates([]PhaseGate{{Require: "does_not_exist:foo"}}, EvidenceContext{})
	if result == nil || !strings.Contains(result.Message, "unknown condition") {
		t.Fatalf("failure = %#v, want unknown condition", result)
	}
}
