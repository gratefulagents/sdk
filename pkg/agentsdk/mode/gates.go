package mode

import (
	"fmt"
	"strings"
)

const (
	GateApproval             = "approval"
	GatePrerequisiteArtifact = "prerequisite_artifact"
	GateSafety               = "safety"
)

const (
	DenyGateFailed       = "GATE_FAILED"
	DenyEdgeNotFound     = "EDGE_NOT_FOUND"
	DenyRBACDenied       = "RBAC_DENIED"
	DenyTemplateNotFound = "TEMPLATE_NOT_FOUND"
)

// GateContext provides the evaluation context for gate checks.
type GateContext struct {
	Run          *RunSnapshot
	TargetStepID string
	Actor        string
	Source       string
}

// GateResult is the outcome of evaluating a single gate.
type GateResult struct {
	Gate     string
	Passed   bool
	Reason   string
	DenyCode string
}

func EvaluateGates(gates []string, gctx GateContext) *GateResult {
	for _, gate := range gates {
		result := evaluateGate(gate, gctx)
		if !result.Passed {
			return &result
		}
	}
	return nil
}

func EvaluateTransitionConditions(conditions []string, gctx GateContext) *GateResult {
	for _, cond := range conditions {
		result := evaluateCondition(cond, gctx)
		if !result.Passed {
			return &result
		}
	}
	return nil
}

func evaluateGate(gate string, gctx GateContext) GateResult {
	switch strings.ToLower(strings.TrimSpace(gate)) {
	case GateApproval:
		return evaluateApprovalGate(gctx)
	case GatePrerequisiteArtifact:
		return GateResult{
			Gate:     GatePrerequisiteArtifact,
			Passed:   false,
			Reason:   "prerequisite artifact gate requires explicit host evidence",
			DenyCode: DenyGateFailed,
		}
	case GateSafety:
		return GateResult{
			Gate:     GateSafety,
			Passed:   false,
			Reason:   "safety gate requires explicit host validation",
			DenyCode: DenyGateFailed,
		}
	default:
		return GateResult{Gate: gate, Passed: false, Reason: fmt.Sprintf("unknown gate %q", gate), DenyCode: DenyGateFailed}
	}
}

func evaluateCondition(cond string, gctx GateContext) GateResult {
	normalized := strings.ToLower(strings.TrimSpace(cond))
	if fn, ok := namedConditions[normalized]; ok {
		return fn(normalized, gctx)
	}
	if prefix, value, ok := parseConditionExpr(normalized); ok {
		if fn, found := prefixConditions[prefix]; found {
			return fn(cond, value, gctx)
		}
	}
	return GateResult{Gate: cond, Passed: false, Reason: fmt.Sprintf("unknown condition %q", cond), DenyCode: DenyGateFailed}
}

func parseConditionExpr(cond string) (string, string, bool) {
	idx := strings.IndexByte(cond, ':')
	if idx < 1 || idx >= len(cond)-1 {
		return "", "", false
	}
	return cond[:idx], cond[idx+1:], true
}

type conditionEvalFn func(name string, gctx GateContext) GateResult

var namedConditions = map[string]conditionEvalFn{
	"subtasks_complete": evalSubtasksComplete,
	"approval_granted":  func(name string, gctx GateContext) GateResult { return evaluateApprovalGate(gctx) },
}

type prefixConditionEvalFn func(rawCond, value string, gctx GateContext) GateResult

var prefixConditions = map[string]prefixConditionEvalFn{
	"phase": func(rawCond, value string, gctx GateContext) GateResult {
		return evalPhaseMatch(rawCond, value, gctx)
	},
}

func evaluateApprovalGate(gctx GateContext) GateResult {
	if gctx.Run == nil {
		return GateResult{Gate: GateApproval, Passed: false, Reason: "no run context", DenyCode: DenyGateFailed}
	}
	if gctx.Source == "system" {
		return GateResult{Gate: GateApproval, Passed: true}
	}
	autonomous := (gctx.Run.ModeSnapshot != nil && gctx.Run.ModeSnapshot.Autonomous) ||
		gctx.Run.WorkflowMode == WorkflowModeAuto
	if autonomous {
		return GateResult{Gate: GateApproval, Passed: true}
	}
	if gctx.Run.Phase == RunPhaseRunning {
		return GateResult{Gate: GateApproval, Passed: true}
	}
	return GateResult{Gate: GateApproval, Passed: false, Reason: "approval required before proceeding", DenyCode: DenyGateFailed}
}

func evalPhaseMatch(name, expected string, gctx GateContext) GateResult {
	if gctx.Run == nil {
		return GateResult{Gate: name, Passed: false, Reason: "no run context", DenyCode: DenyGateFailed}
	}
	if strings.EqualFold(string(gctx.Run.Phase), expected) {
		return GateResult{Gate: name, Passed: true}
	}
	return GateResult{Gate: name, Passed: false, Reason: fmt.Sprintf("phase is %q, want %q", gctx.Run.Phase, expected), DenyCode: DenyGateFailed}
}

func evalSubtasksComplete(name string, gctx GateContext) GateResult {
	if gctx.Run == nil {
		return GateResult{Gate: name, Passed: false, Reason: "no run context", DenyCode: DenyGateFailed}
	}
	if !gctx.Run.CompletionRequested {
		return GateResult{Gate: name, Passed: false, Reason: "completion not yet requested by agent", DenyCode: DenyGateFailed}
	}
	if len(gctx.Run.Children) == 0 {
		return GateResult{Gate: name, Passed: false, Reason: "no child runs found", DenyCode: DenyGateFailed}
	}
	for _, child := range gctx.Run.Children {
		if child.Phase != RunPhaseSucceeded && child.Phase != RunPhaseFailed {
			return GateResult{Gate: name, Passed: false, Reason: fmt.Sprintf("child %q still in phase %q", child.Name, child.Phase), DenyCode: DenyGateFailed}
		}
	}
	return GateResult{Gate: name, Passed: true}
}

func FormatDenialReason(result *GateResult) string {
	if result == nil {
		return ""
	}
	if result.DenyCode != "" {
		return fmt.Sprintf("[%s] %s: %s", result.DenyCode, result.Gate, result.Reason)
	}
	return fmt.Sprintf("%s: %s", result.Gate, result.Reason)
}
