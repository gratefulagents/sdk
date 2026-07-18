package agentsdk

import (
	"context"
	"fmt"
	"strings"
)

// DefaultCriticInstructions is the system prompt used by NewCriticVerifier
// when the critic agent has no instructions of its own. The critic's job is
// adversarial: actively try to refute the candidate answer rather than
// rubber-stamp it.
const DefaultCriticInstructions = `You are an independent reviewer. Another agent claims to have completed a task; your job is to try to REFUTE that claim using read-only inspection.

Actively look for: requirements that were not satisfied, files or artifacts that should exist but do not (or have wrong content), claims in the answer contradicted by the actual state of the workspace, tests/checks that were never run, and unhandled edge cases the task implies.

Verify evidence with your tools instead of trusting the answer's claims. Be precise and cite file paths or command output for every problem you report.

End your reply with exactly one verdict line:
VERDICT: APPROVED — only if you could not find any substantive problem.
VERDICT: REJECTED — followed by a numbered list of concrete, actionable problems.
Do not reject for style, phrasing, or hypothetical concerns you did not verify.`

// criticVerdictInconclusive is returned as feedback when the critic produced
// no usable verdict (empty reply, exhausted turn budget, or no verdict line).
// The verifier runs once per finalization attempt, so this costs at most one
// extra round instead of silently approving.
const criticVerdictInconclusive = "The independent verifier did not return a usable verdict. Re-check your answer against the original task requirements, verify your claims, and provide your final answer again."

// criticVerdictApproves parses the critic's reply for verdict lines. Only a
// line that is exactly "VERDICT: APPROVED" (anchored, case-insensitive,
// optional trailing period) counts as approval, and it must be the last
// verdict line with no rejection anywhere — quoting the rubric or discussing
// the verdict format cannot approve. The second return reports whether any
// verdict line was found at all.
func criticVerdictApproves(reply string) (approved bool, found bool) {
	sawRejected := false
	lastApproved := false
	for _, line := range strings.Split(reply, "\n") {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if !strings.HasPrefix(upper, "VERDICT:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(upper, "VERDICT:"))
		switch {
		case rest == "APPROVED" || rest == "APPROVED.":
			found = true
			lastApproved = true
		case strings.HasPrefix(rest, "REJECTED"):
			found = true
			lastApproved = false
			sawRejected = true
		}
	}
	return lastApproved && !sawRejected, found
}

// NewCriticVerifier builds a RunConfig.FinalAnswerVerifier backed by a
// read-only critic sub-agent that attempts to refute the candidate final
// answer against the original task (adversarial verification). The critic
// runs with read-only tool access regardless of the tools on the supplied
// agent. If the critic approves (or fails to run), no feedback is returned
// and finalization proceeds.
func NewCriticVerifier(runner *Runner, critic *Agent, originalTask string) func(ctx context.Context, finalText string) (string, error) {
	return func(ctx context.Context, finalText string) (string, error) {
		if runner == nil || critic == nil {
			return "", fmt.Errorf("critic verifier requires a runner and a critic agent")
		}
		criticAgent := critic.Clone()
		if strings.TrimSpace(criticAgent.Instructions) == "" {
			criticAgent.Instructions = DefaultCriticInstructions
		}
		prompt := fmt.Sprintf(`<original_task>
%s
</original_task>

<candidate_final_answer>
%s
</candidate_final_answer>

Review the candidate final answer against the original task. Verify its claims with your tools, then give your verdict.`, originalTask, finalText)

		result, err := runner.Run(ctx, criticAgent, []RunItem{{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: prompt},
		}}, RunConfig{
			MaxTurns:              12,
			SubAgentMaxTurns:      12,
			ToolAccessLevel:       ToolAccessLevelReadOnly,
			ForceFinalSummaryTurn: true,
		})
		if err != nil {
			return "", err
		}
		verdict := result.FinalText()
		approved, found := criticVerdictApproves(verdict)
		if approved {
			return "", nil
		}
		if !found || strings.TrimSpace(verdict) == "" {
			return criticVerdictInconclusive, nil
		}
		return verdict, nil
	}
}
