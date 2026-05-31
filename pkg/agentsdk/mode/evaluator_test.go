package mode

import "testing"

func TestEvaluateGatesFailClosedForPlaceholderAndUnknownGates(t *testing.T) {
	for _, gate := range []string{GatePrerequisiteArtifact, GateSafety, "does_not_exist"} {
		result := EvaluateGates([]string{gate}, GateContext{})
		if result == nil || result.Passed || result.DenyCode != DenyGateFailed {
			t.Fatalf("EvaluateGates(%q) = %#v, want fail-closed denial", gate, result)
		}
	}
}

func TestEvaluateDeniesMissingTransitionEdge(t *testing.T) {
	current := &TemplateSpec{
		Name: "plan",
		Transitions: []Transition{{
			From: "plan",
			To:   "review",
		}},
	}
	target := &TemplateSpec{Name: "execute"}

	result := Evaluate(current, target, EvaluateOpts{ActorRole: RoleSystem})
	if result.Result != ResultDenied || result.DenyCode != DenyEdgeNotFound {
		t.Fatalf("Evaluate() = %#v, want missing edge denial", result)
	}
}

func TestEvaluateAppliesTransitionGates(t *testing.T) {
	current := &TemplateSpec{
		Name: "plan",
		Transitions: []Transition{{
			From:  "plan",
			To:    "execute",
			Gates: []string{GateSafety},
		}},
	}
	target := &TemplateSpec{Name: "execute"}

	result := Evaluate(current, target, EvaluateOpts{ActorRole: RoleSystem})
	if result.Result != ResultDenied || result.DenyCode != DenyGateFailed {
		t.Fatalf("Evaluate() = %#v, want safety gate denial", result)
	}
}

func TestEvaluateAllowsMatchingTransitionConditions(t *testing.T) {
	current := &TemplateSpec{
		Name: "plan",
		Transitions: []Transition{{
			From: "plan",
			To:   "execute",
			When: []string{"phase:Running"},
		}},
	}
	target := &TemplateSpec{Name: "execute"}

	result := Evaluate(current, target, EvaluateOpts{
		ActorRole: RoleSystem,
		Run:       &RunSnapshot{Phase: RunPhaseRunning},
	})
	if result.Result != ResultApplied {
		t.Fatalf("Evaluate() = %#v, want transition applied", result)
	}
}
