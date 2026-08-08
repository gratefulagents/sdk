package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// subAgentRunSpec describes one nested sub-agent execution for runSubAgentOnce.
// It is the single execution engine shared by the async task scheduler
// (SubAgentRegistry.runTask) and the sync agent-as-tool wrapper (Agent.AsTool),
// so lifecycle events, context injection, and outcome accounting cannot drift
// between delegation surfaces.
type subAgentRunSpec struct {
	Runner  *Runner
	Agent   *Agent
	Message string
	// InitialContext is optional parent conversation history copied before the
	// child's task message. It is used only by explicitly context-sharing
	// delegation surfaces.
	InitialContext []RunItem
	TaskID         string
	ParentCallID   string
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
	if spec.TaskID == "" {
		spec.TaskID = "task_" + uuid.NewString()
	}
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
	contextSuffix := ""
	if spec.RunConfig.WorkDir != "" {
		contextSuffix += "\n\n" + BuildWorkspaceContext(spec.RunConfig.WorkDir, spec.RunConfig.ToolAccessLevel)
	}
	contextSuffix += "\n\n" + BuildSubAgentBudgetContext(spec.RunConfig.MaxTurns)
	childAgent.Instructions = childAgent.Instructions + contextSuffix
	// GetInstructions prefers InstructionsFn, so dynamic-instruction agents
	// must have the context appended to the function output too.
	if origFn := childAgent.InstructionsFn; origFn != nil {
		childAgent.InstructionsFn = func(ctx *RunContext, a *Agent) string {
			return origFn(ctx, a) + contextSuffix
		}
	}

	cfg := spec.RunConfig
	cfg.PromptCacheKey = spec.TaskID
	cfg.Hooks = hooks
	cfg.ForceFinalSummaryTurn = true

	items := cloneParentRunItems(spec.InitialContext)
	items = append(items, RunItem{
		Type:    RunItemMessage,
		Message: &MessageOutput{Text: spec.Message},
	})

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
		// Partial-result semantics: a budget-exhausted child attaches its
		// accumulated run state to the error, while cancellations and
		// model/API failures hand it back as the result. Surface the child's
		// last assistant message either way so the parent receives the
		// findings gathered so far instead of a bare failure.
		partial := result
		var budgetErr *MaxTurnsExceeded
		if errors.As(err, &budgetErr) && budgetErr.PartialResult != nil {
			partial = budgetErr.PartialResult
		}
		if tail := partialProgressTail(partial); tail != "" {
			outcome.ErrMsg += "\nPartial progress before the run ended:\n" + TruncateMiddle(tail, 1600)
		}
		// Runner.Run returns a nil result on most errors (max-turns hands
		// back a partial result, but without provider-reported totals), so
		// recover the partial usage the child tracker observed (its own LLM
		// calls plus anything forwarded from nested subagents) for outcome
		// accounting and the completion event/span. Run-level totals are
		// unaffected either way: per-call forwarding already recorded this
		// usage on the parent.
		if childTracker != nil {
			snap := childTracker.Snapshot()
			outcome.CostUSD = snap.CostUsd
			outcome.CostKnown = snap.CostUsd > 0
			outcome.Usage = Usage{
				InputTokens:       snap.InputTokens,
				OutputTokens:      snap.OutputTokens,
				CacheReadTokens:   snap.CacheReadInputTokens,
				CacheCreateTokens: snap.CacheCreationInputTokens,
			}
			outcome.Tokens = snap.InputTokens + snap.OutputTokens
			outcome.ToolCount = snap.ToolCallCount
		}
		if spec.OnTerminal != nil {
			spec.OnTerminal(outcome)
		}
		filesRead, filesWritten := spec.activityFiles()
		if spec.Tracker != nil {
			spec.Tracker.RecordSubagentCompleted(spec.TaskID, outcome.Status, outcome.ErrMsg, outcome.CostUSD, 0, outcome.Usage, "", filesRead, filesWritten)
		}
		if spec.EventStream != nil {
			spec.EventStream.EmitSubagentCompletedWithUsage(spec.TaskID, outcome.Status, outcome.ErrMsg, outcome.ToolCount, outcome.Tokens, outcome.Duration.Milliseconds(), outcome.CostUSD, outcome.CostKnown, 0, outcome.Usage, outcome.Status, "")
		}
		return outcome
	}

	outcome.FinalText = result.FinalText()
	if outcome.FinalText == "" {
		outcome.FinalText = "(no output)"
	}
	for _, item := range result.FinalHistory {
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
		spec.EventStream.EmitSubagentCompletedWithUsage(spec.TaskID, outcome.Status, outcome.FinalText, outcome.ToolCount, outcome.Tokens, outcome.Duration.Milliseconds(), outcome.CostUSD, outcome.CostKnown, int32(outcome.NumTurns), outcome.Usage, "", outcome.FinalText)
	}
	return outcome
}

// partialProgressTail extracts the last assistant message text from a
// partial run result so a budget-exhausted child still reports what it
// learned before the cap instead of returning a bare failure.
func partialProgressTail(result *RunResult) string {
	if result == nil {
		return ""
	}
	for i := len(result.NewItems) - 1; i >= 0; i-- {
		item := result.NewItems[i]
		if item.Type != RunItemMessage || item.Message == nil {
			continue
		}
		text := strings.TrimSpace(item.Message.Text)
		if text == "" || strings.HasPrefix(text, "[SYSTEM]") {
			continue
		}
		return text
	}
	return ""
}
