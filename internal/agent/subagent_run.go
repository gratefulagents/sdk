package agent

import (
	"context"
	"fmt"
	"time"
)

// subAgentRunSpec describes one nested sub-agent execution for runSubAgentOnce.
// It is the single execution engine shared by the async task scheduler
// (SubAgentRegistry.runTask) and the sync agent-as-tool wrapper (Agent.AsTool),
// so lifecycle events, context injection, and outcome accounting cannot drift
// between delegation surfaces.
type subAgentRunSpec struct {
	Runner       *Runner
	Agent        *Agent
	Message      string
	TaskID       string
	ParentCallID string
	// Isolation labels the run for progress records ("" = inline tool call,
	// "async" = managed background task).
	Isolation string

	Tracker     *ProgressTracker
	EventStream *EventStream
	// Activity, when set, is wired into the child hooks so the parent can
	// inspect tool/file activity while the task runs.
	Activity *SubAgentActivity
	// Turn is copied onto the child PlatformHooks so child tool events carry
	// the parent's conversation turn.
	Turn int
	// FallbackHooks are used when neither Tracker nor EventStream is set.
	FallbackHooks RunHooks

	// OnTerminal, when set, runs after outcome classification but before
	// completion events are emitted, so callers can commit terminal state
	// (e.g. registry task status) before hosts observe the completion event.
	OnTerminal func(subAgentOutcome)

	// RunConfig is the fully assembled nested run configuration except Hooks
	// and ForceFinalSummaryTurn, which the engine owns. WorkDir, MaxTurns and
	// ToolAccessLevel must already be resolved by the caller.
	RunConfig RunConfig
}

// subAgentOutcome is the uniform result of one nested sub-agent execution.
type subAgentOutcome struct {
	Result *RunResult
	Err    error
	// Status is "completed", "stopped" (interrupted), "failed", or "cancelled".
	Status    string
	FinalText string
	ErrMsg    string
	Duration  time.Duration
	ToolCount int32
	Tokens    int64
	Usage     Usage
	CostUSD   float64
	CostKnown bool
	NumTurns  int
}

// Terminal task status labels shared by outcome classification.
const (
	subAgentStatusCompleted = "completed"
	subAgentStatusStopped   = "stopped"
	subAgentStatusFailed    = "failed"
	subAgentStatusCancelled = "cancelled"
)

// activityFiles returns the file activity recorded so far, if a ledger is wired.
func (s subAgentRunSpec) activityFiles() (filesRead, filesWritten []string) {
	if s.Activity == nil {
		return nil, nil
	}
	snapshot := s.Activity.Snapshot(false)
	return snapshot.FilesRead, snapshot.FilesWritten
}

// runSubAgentOnce executes a nested sub-agent run: it emits start events,
// builds child hooks, clones the agent with workspace/budget context, runs it,
// classifies the outcome, and emits completion events. Registry bookkeeping
// (task status, semaphores, dependency waits) stays with the caller.
func runSubAgentOnce(ctx context.Context, spec subAgentRunSpec) subAgentOutcome {
	description := Truncate(spec.Message, 160)

	var childTracker *ProgressTracker
	if spec.Tracker != nil {
		spec.Tracker.RecordSubagentStarted(spec.TaskID, spec.ParentCallID, description, spec.Agent.Name, spec.Agent.Model, spec.Isolation, spec.Message)
		childTracker = NewChildTracker(spec.Tracker, spec.TaskID)
		if spec.RunConfig.ParentSpanID != "" {
			childTracker.SetRootSpanID(spec.RunConfig.ParentSpanID)
		}
	}
	var childES *EventStream
	if spec.EventStream != nil {
		spec.EventStream.EmitSubagentStarted(spec.TaskID, spec.ParentCallID, description, spec.Agent.Name, spec.Agent.Model, spec.Message)
		childES = NewChildEventStream(spec.EventStream, spec.TaskID)
	}

	hooks := spec.FallbackHooks
	if childTracker != nil || childES != nil {
		platformHooks := NewPlatformHooks(childTracker, childES)
		platformHooks.Turn = spec.Turn
		platformHooks.Activity = spec.Activity
		hooks = platformHooks
	}

	// Clone the agent and inject workspace context into instructions so the
	// sub-agent knows its working directory and available tool capabilities.
	// Without this, models guess wrong absolute paths, get "outside workspace"
	// errors on reads, and incorrectly conclude they lack write tools.
	childAgent := spec.Agent.Clone()
	if spec.RunConfig.WorkDir != "" {
		childAgent.Instructions = childAgent.Instructions + "\n\n" + BuildWorkspaceContext(spec.RunConfig.WorkDir, spec.RunConfig.ToolAccessLevel)
	}
	childAgent.Instructions = childAgent.Instructions + "\n\n" + BuildSubAgentBudgetContext(spec.RunConfig.MaxTurns)

	cfg := spec.RunConfig
	cfg.Hooks = hooks
	cfg.ForceFinalSummaryTurn = true

	items := []RunItem{{
		Type:    RunItemMessage,
		Message: &MessageOutput{Text: spec.Message},
	}}

	startedAt := time.Now()
	childCtx := WithTaskID(ctx, spec.TaskID)
	result, err := spec.Runner.Run(childCtx, childAgent, items, cfg)
	outcome := subAgentOutcome{
		Result:   result,
		Err:      err,
		Duration: time.Since(startedAt),
	}

	if err != nil {
		// Distinguish context cancellation (caller-initiated) from failures.
		outcome.Status = subAgentStatusFailed
		if ctx.Err() != nil {
			outcome.Status = subAgentStatusCancelled
		}
		outcome.ErrMsg = fmt.Sprintf("agent %q %s: %v", spec.Agent.Name, outcome.Status, err)
		if spec.OnTerminal != nil {
			spec.OnTerminal(outcome)
		}
		filesRead, filesWritten := spec.activityFiles()
		if spec.Tracker != nil {
			spec.Tracker.RecordSubagentCompleted(spec.TaskID, outcome.Status, outcome.ErrMsg, 0, 0, Usage{}, "", filesRead, filesWritten)
		}
		if spec.EventStream != nil {
			spec.EventStream.EmitSubagentCompleted(spec.TaskID, outcome.Status, outcome.ErrMsg, 0, 0, outcome.Duration.Milliseconds(), 0, false, 0, outcome.Status, "")
		}
		return outcome
	}

	outcome.FinalText = result.FinalText()
	if outcome.FinalText == "" {
		outcome.FinalText = "(no output)"
	}
	for _, item := range result.NewItems {
		if item.Type == RunItemToolCall && item.ToolCall != nil {
			outcome.ToolCount++
		}
	}
	outcome.Usage = Usage{
		InputTokens:       result.Usage.InputTokens,
		OutputTokens:      result.Usage.OutputTokens,
		CacheReadTokens:   result.Usage.CacheReadTokens,
		CacheCreateTokens: result.Usage.CacheCreateTokens,
	}
	outcome.Tokens = result.Usage.InputTokens + result.Usage.OutputTokens
	outcome.CostUSD, outcome.CostKnown = estimateRunResultCost(result, spec.Runner.model)
	outcome.NumTurns = len(result.RawResponses)
	outcome.Status = subAgentStatusCompleted
	if result.Interruption != nil {
		outcome.Status = subAgentStatusStopped
	}
	if spec.OnTerminal != nil {
		spec.OnTerminal(outcome)
	}

	var filesRead, filesWritten []string
	if spec.Activity != nil {
		filesRead, filesWritten = spec.activityFiles()
	}
	if spec.Tracker != nil {
		spec.Tracker.RecordSubagentProgress(spec.TaskID, outcome.ToolCount, outcome.Tokens, outcome.Duration.Milliseconds(), "")
		spec.Tracker.RecordSubagentCompleted(
			spec.TaskID, outcome.Status, outcome.FinalText,
			outcome.CostUSD,
			outcome.NumTurns,
			outcome.Usage, "",
			filesRead, filesWritten,
		)
	}
	if spec.EventStream != nil {
		spec.EventStream.EmitSubagentCompleted(spec.TaskID, outcome.Status, outcome.FinalText, outcome.ToolCount, outcome.Tokens, outcome.Duration.Milliseconds(), outcome.CostUSD, outcome.CostKnown, int32(outcome.NumTurns), "", outcome.FinalText)
	}
	return outcome
}
