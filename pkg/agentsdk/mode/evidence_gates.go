package mode

import (
	"fmt"
	"os/exec"
	"strings"
)

// EvidenceContext provides state for phase entry gate evaluation.
type EvidenceContext struct {
	WorkDir    string
	PlanExists bool
}

// GitRunner executes a git command in the given directory.
type GitRunner func(workDir string, args ...string) ([]byte, error)

func DefaultGitRunner(workDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	return cmd.CombinedOutput()
}

// PhaseGateResult is the outcome of evaluating a single phase entry gate.
type PhaseGateResult struct {
	Require string
	Passed  bool
	Message string
}

func EvaluatePhaseEntryGates(gates []PhaseGate, ctx EvidenceContext) *PhaseGateResult {
	return EvaluatePhaseEntryGatesWithGitRunner(gates, ctx, DefaultGitRunner)
}

func EvaluatePhaseEntryGatesWithGitRunner(gates []PhaseGate, ctx EvidenceContext, gitRunner GitRunner) *PhaseGateResult {
	if gitRunner == nil {
		gitRunner = DefaultGitRunner
	}
	for _, gate := range gates {
		condition, args := parseRequire(gate.Require)
		passed, reason := evaluateEntryGateCondition(condition, args, ctx, gitRunner)
		if !passed {
			msg := gate.Message
			if msg == "" {
				msg = reason
			}
			return &PhaseGateResult{Require: gate.Require, Passed: false, Message: msg}
		}
	}
	return nil
}

func parseRequire(require string) (string, string) {
	idx := strings.IndexByte(require, ':')
	if idx < 0 {
		return strings.TrimSpace(require), ""
	}
	return strings.TrimSpace(require[:idx]), strings.TrimSpace(require[idx+1:])
}

func evaluateEntryGateCondition(condition, args string, ctx EvidenceContext, gitRunner GitRunner) (bool, string) {
	_ = args
	switch condition {
	case "git_clean":
		return evalGitClean(ctx, gitRunner)
	case "plan_exists":
		return evalPlanExists(ctx)
	default:
		return false, fmt.Sprintf("unknown condition %q", condition)
	}
}

func evalGitClean(ctx EvidenceContext, gitRunner GitRunner) (bool, string) {
	out, err := gitRunner(ctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Sprintf("git status failed: %v", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return true, "working tree clean"
	}
	lines := strings.Count(trimmed, "\n") + 1
	return false, fmt.Sprintf("%d uncommitted change(s)", lines)
}

func evalPlanExists(ctx EvidenceContext) (bool, string) {
	if ctx.PlanExists {
		return true, "plan artifact exists"
	}
	return false, "no plan artifact found"
}
