package agent

import (
	"context"
	"encoding/json"
)

// ActionDecision is the outcome of action authorization.
type ActionDecision string

const (
	ActionDecisionAllow    ActionDecision = "allow"
	ActionDecisionDeny     ActionDecision = "deny"
	ActionDecisionAsk      ActionDecision = "ask"
	ActionDecisionClassify ActionDecision = "classify"
)

// ActionRisk describes the risk level assigned to an action.
type ActionRisk string

const (
	ActionRiskLow    ActionRisk = "low"
	ActionRiskMedium ActionRisk = "medium"
	ActionRiskHigh   ActionRisk = "high"
)

// ActionRequest describes an action submitted for authorization.
type ActionRequest struct {
	ToolName   string          `json:"tool_name"`
	Input      json.RawMessage `json:"input"`
	WorkDir    string          `json:"work_dir,omitempty"`
	ReadOnly   bool            `json:"read_only"`
	Provenance string          `json:"provenance,omitempty"`
}

// ActionAuthorization is an authorizer's decision for an action.
type ActionAuthorization struct {
	Decision ActionDecision `json:"decision"`
	Reason   string         `json:"reason,omitempty"`
	Rule     string         `json:"rule,omitempty"`
	Risk     ActionRisk     `json:"risk,omitempty"`
}

// ActionAuthorizer authorizes an action before it is executed.
type ActionAuthorizer interface {
	Authorize(context.Context, ActionRequest) (ActionAuthorization, error)
}

// ActionAuditRecord records an action authorization decision.
type ActionAuditRecord struct {
	Request       ActionRequest       `json:"request"`
	Authorization ActionAuthorization `json:"authorization"`
	Error         string              `json:"error,omitempty"`
}

const (
	actionAuthorizationErrorRule     = "authorizer-error"
	actionAuthorizationErrorReason   = "action authorizer failed"
	actionAuthorizationInvalidRule   = "invalid-decision"
	actionAuthorizationInvalidReason = "action authorizer returned an invalid decision"
)

// NormalizeActionAuthorization fails closed for authorizer errors and invalid terminal decisions.
func NormalizeActionAuthorization(authorization ActionAuthorization, err error) ActionAuthorization {
	if err != nil {
		return ActionAuthorization{
			Decision: ActionDecisionDeny,
			Reason:   actionAuthorizationErrorReason,
			Rule:     actionAuthorizationErrorRule,
			Risk:     authorization.Risk,
		}
	}

	switch authorization.Decision {
	case ActionDecisionAllow, ActionDecisionDeny, ActionDecisionAsk:
		return authorization
	default:
		return ActionAuthorization{
			Decision: ActionDecisionDeny,
			Reason:   actionAuthorizationInvalidReason,
			Rule:     actionAuthorizationInvalidRule,
			Risk:     authorization.Risk,
		}
	}
}
