package mode

import (
	"fmt"
	"strings"
)

// Role is the actor role used for mode RBAC.
type Role string

const (
	RoleSystem Role = "system"
	RoleAdmin  Role = "admin"
	RoleUser   Role = "user"
)

// TransitionResult is the outcome of evaluating a mode transition.
type TransitionResult string

const (
	ResultApplied    TransitionResult = "applied"
	ResultDenied     TransitionResult = "denied"
	ResultNoop       TransitionResult = "noop"
	ResultRolledBack TransitionResult = "rolled_back"
)

type EvaluateResult struct {
	Result   TransitionResult
	Reason   string
	DenyCode string
	Target   *TemplateSpec
}

type EvaluateOpts struct {
	Run       *RunSnapshot
	ActorRole Role
	Source    string
}

// Evaluate checks whether switching from current to target is valid.
func Evaluate(current *TemplateSpec, target *TemplateSpec, opts ...EvaluateOpts) EvaluateResult {
	if target == nil {
		return EvaluateResult{
			Result:   ResultDenied,
			Reason:   fmt.Sprintf("[%s] target template is nil", DenyTemplateNotFound),
			DenyCode: DenyTemplateNotFound,
		}
	}
	if current != nil && current.Name == target.Name {
		return EvaluateResult{Result: ResultNoop, Reason: "already in requested mode"}
	}
	var o EvaluateOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	if len(opts) > 0 {
		minRole := minRoleForCategory(target.Category)
		if o.ActorRole != RoleSystem && roleRank(o.ActorRole) < roleRank(minRole) {
			rbacResult := &GateResult{
				Gate:     "rbac",
				Passed:   false,
				Reason:   fmt.Sprintf("role %q insufficient for mode %q (requires %q)", o.ActorRole, target.Name, minRole),
				DenyCode: DenyRBACDenied,
			}
			return EvaluateResult{Result: ResultDenied, Reason: FormatDenialReason(rbacResult), DenyCode: rbacResult.DenyCode}
		}
	}
	if current != nil && len(current.Transitions) > 0 {
		transition, ok := findTransition(current, target)
		if !ok {
			gateResult := &GateResult{
				Gate:     "transition",
				Passed:   false,
				Reason:   fmt.Sprintf("no transition from %q to %q", current.Name, target.Name),
				DenyCode: DenyEdgeNotFound,
			}
			return EvaluateResult{Result: ResultDenied, Reason: FormatDenialReason(gateResult), DenyCode: gateResult.DenyCode}
		}
		gctx := GateContext{Run: o.Run, Actor: string(o.ActorRole), Source: o.Source}
		if result := EvaluateTransitionConditions(transition.When, gctx); result != nil {
			return EvaluateResult{Result: ResultDenied, Reason: FormatDenialReason(result), DenyCode: result.DenyCode}
		}
		if result := EvaluateGates(transition.Gates, gctx); result != nil {
			return EvaluateResult{Result: ResultDenied, Reason: FormatDenialReason(result), DenyCode: result.DenyCode}
		}
	}
	return EvaluateResult{Result: ResultApplied, Target: target}
}

func findTransition(current *TemplateSpec, target *TemplateSpec) (Transition, bool) {
	if current == nil || target == nil {
		return Transition{}, false
	}
	for _, transition := range current.Transitions {
		if transitionMatches(transition.From, current.Name) && transitionMatches(transition.To, target.Name) {
			return transition, true
		}
	}
	return Transition{}, false
}

func transitionMatches(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" {
		return true
	}
	return strings.EqualFold(pattern, name)
}

func minRoleForCategory(category string) Role {
	switch category {
	case "orchestrated":
		return RoleAdmin
	default:
		return RoleUser
	}
}

func roleRank(role Role) int {
	switch role {
	case RoleSystem:
		return 3
	case RoleAdmin:
		return 2
	case RoleUser:
		return 1
	default:
		return 0
	}
}
