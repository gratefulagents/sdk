package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AsTool returns a Tool that runs this agent as a nested sub-agent.
// When called, the tool input becomes the user message and the agent's
// final text output becomes the tool result. Options can override the tool
// name/description or post-process the sub-agent result before it is returned
// to the parent (custom output extraction).
func (a *Agent) AsTool(runner *Runner, opts ...AsToolOption) Tool {
	t := &agentTool{agent: a, runner: runner}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// AsToolOption configures an agent-as-tool wrapper produced by Agent.AsTool.
type AsToolOption func(*agentTool)

// WithAsToolName overrides the generated tool name for an agent-as-tool wrapper.
func WithAsToolName(name string) AsToolOption {
	return func(t *agentTool) { t.nameOverride = name }
}

// WithAsToolDescription overrides the tool description for an agent-as-tool wrapper.
func WithAsToolDescription(desc string) AsToolOption {
	return func(t *agentTool) { t.descriptionOverride = desc }
}

// WithAsToolOutputExtractor sets a function that transforms the sub-agent's
// RunResult into the string returned to the parent agent. Use it to return a
// compact, structured, or filtered view of the sub-agent's work instead of its
// raw final text. If the extractor returns an empty string, the wrapper falls
// back to the sub-agent's final text.
func WithAsToolOutputExtractor(fn func(*RunResult) string) AsToolOption {
	return func(t *agentTool) { t.outputExtractor = fn }
}

// agentTool wraps an Agent as a Tool for agent composition.
type agentTool struct {
	agent               *Agent
	runner              *Runner
	nameOverride        string
	descriptionOverride string
	outputExtractor     func(*RunResult) string
}

func (t *agentTool) Name() string {
	if t.nameOverride != "" {
		return t.nameOverride
	}
	return "agent_" + safeAgentToolName(t.agent.Name)
}
func (t *agentTool) Description() string {
	if t.descriptionOverride != "" {
		return t.descriptionOverride
	}
	return t.agent.HandoffDescription
}
func (t *agentTool) IsReadOnly() bool {
	// A sub-agent tool is read-only if all its tools are read-only.
	for _, tool := range t.agent.Tools {
		if !tool.IsReadOnly() {
			return false
		}
	}
	return true
}

func safeAgentToolName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "agent"
	}
	return out
}
func (t *agentTool) IsEnabled(ctx *RunContext) bool {
	if ctx == nil {
		return true
	}
	switch NormalizeToolAccessLevel(ctx.ToolAccessLevel) {
	case ToolAccessLevelReadOnly:
		return t.IsReadOnly()
	default:
		return true
	}
}
func (t *agentTool) NeedsApproval() bool { return false }
func (t *agentTool) TimeoutSeconds() int { return 0 }

func (t *agentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"The ONLY context the sub-agent receives. Send a compact task packet: exact task, goal, only the relevant repo context, constraints/acceptance criteria, prior findings this task depends on, and expected output. Avoid unrelated history."}},"required":["message"]}`)
}

func (t *agentTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (ToolResult, error) {
	// Parse the message from the tool input.
	var params struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	if params.Message == "" {
		return ToolResult{Content: "message is required", IsError: true}, nil
	}

	// Clone the agent; workspace context is injected after the child tool
	// access level is resolved below (the nested run config may clamp it).
	childAgent := t.agent.Clone()

	// Run the nested agent, inheriting the runner's default hooks so sub-agent
	// tool events appear in the parent's activity log.
	items := []RunItem{{
		Type:    RunItemMessage,
		Message: &MessageOutput{Text: params.Message},
	}}
	runHooks := t.runner.DefaultHooks
	var (
		parentTracker *ProgressTracker
		parentES      *EventStream
		taskID        string
		trace         = TraceFromContext(ctx)
		processor     = TracingProcessorFromContext(ctx)
		parentSpanID  = SpanParentIDFromContext(ctx)
	)
	if platformHooks, ok := t.runner.DefaultHooks.(*PlatformHooks); ok && platformHooks != nil && platformHooks.Tracker != nil {
		parentTracker = platformHooks.Tracker
		parentES = platformHooks.EventStream
		taskID = "task_" + uuid.NewString()
		toolUseID := ParentCallIDFromContext(ctx)
		description := Truncate(params.Message, 160)
		parentTracker.RecordSubagentStarted(taskID, toolUseID, description, t.agent.Name, t.agent.Model, "", params.Message)

		// Emit subagent start to EventStream for host activity views.
		if parentES != nil {
			parentES.EmitSubagentStarted(taskID, toolUseID, description, t.agent.Name, t.agent.Model, params.Message)
		}

		childTracker := NewChildTracker(parentTracker, taskID)
		if parentSpanID != "" {
			childTracker.SetRootSpanID(parentSpanID)
		}
		var childES *EventStream
		if parentES != nil {
			childES = NewChildEventStream(parentES, taskID)
		}
		childHooks := NewPlatformHooks(childTracker, childES)
		childHooks.Turn = platformHooks.Turn
		runHooks = childHooks
	}

	startedAt := time.Now()
	childCtx := WithTaskID(ctx, taskID)
	runConfig := RunConfig{
		MaxTurns:              DefaultSubAgentMaxTurns,
		SubAgentMaxTurns:      DefaultSubAgentMaxTurns,
		Hooks:                 runHooks,
		WorkDir:               workDir,
		Trace:                 trace,
		ParentSpanID:          parentSpanID,
		TracingProcessor:      processor,
		ForceFinalSummaryTurn: true,
	}
	// Child tool access defaults to the wrapper's effective access; the parent
	// run's level (inherited below) can only clamp it further, never widen it.
	childToolAccess := ToolAccessLevelFull
	if t.IsReadOnly() {
		childToolAccess = ToolAccessLevelReadOnly
	}
	if nestedCfg, ok := NestedRunConfigFromContext(ctx); ok {
		runConfig.MaxTurns = nestedCfg.MaxTurns
		runConfig.SubAgentMaxTurns = nestedCfg.MaxTurns
		runConfig.CompactionConfig = nestedCfg.CompactionConfig
		runConfig.CompactionRecorder = nestedCfg.CompactionRecorder
		runConfig.CompactionFailureReporter = nestedCfg.CompactionFailureReporter
		runConfig.HandoffHistory = nestedCfg.HandoffHistory
		// Inherit the parent's tool policy and security settings so nested
		// runs cannot bypass approval requirements, untrusted-output tagging,
		// or output caps that the host configured on the parent run.
		runConfig.ToolPolicy = nestedCfg.ToolPolicy
		runConfig.UntrustedToolOutputs = nestedCfg.UntrustedToolOutputs
		runConfig.MaxToolOutputBytes = nestedCfg.MaxToolOutputBytes
		if NormalizeToolAccessLevel(nestedCfg.ToolAccessLevel) == ToolAccessLevelReadOnly {
			childToolAccess = ToolAccessLevelReadOnly
		}
	}
	runConfig.ToolAccessLevel = childToolAccess
	if workDir != "" {
		childAgent.Instructions = childAgent.Instructions + "\n\n" + BuildWorkspaceContext(workDir, childToolAccess)
	}
	childAgent.Instructions = childAgent.Instructions + "\n\n" + BuildSubAgentBudgetContext(runConfig.MaxTurns)
	result, err := t.runner.Run(childCtx, childAgent, items, runConfig)
	if err != nil {
		if parentTracker != nil {
			parentTracker.RecordSubagentCompleted(taskID, "failed", fmt.Sprintf("agent %q failed: %v", t.agent.Name, err), 0, 0, Usage{}, "", nil, nil)
		}
		if parentES != nil {
			parentES.EmitSubagentCompleted(taskID, "failed", fmt.Sprintf("agent %q failed: %v", t.agent.Name, err), 0, 0, 0, 0, false, 0, "error", "")
		}
		return ToolResult{Content: fmt.Sprintf("agent %q failed: %v", t.agent.Name, err), IsError: true}, nil
	}

	if parentTracker != nil {
		usage := Usage{
			InputTokens:       result.Usage.InputTokens,
			OutputTokens:      result.Usage.OutputTokens,
			CacheReadTokens:   result.Usage.CacheReadTokens,
			CacheCreateTokens: result.Usage.CacheCreateTokens,
		}
		toolCount := int32(0)
		for _, item := range result.NewItems {
			if item.Type == RunItemToolCall && item.ToolCall != nil {
				toolCount++
			}
		}
		parentTracker.RecordSubagentProgress(taskID, toolCount, usage.InputTokens+usage.OutputTokens, time.Since(startedAt).Milliseconds(), "")

		status := "completed"
		if result.Interruption != nil {
			status = "stopped"
		}
		summary := result.FinalText()
		if summary == "" {
			summary = "(no output)"
		}
		costUsd, costKnown := estimateRunResultCost(result, t.runner.model)
		numTurns := len(result.RawResponses)
		parentTracker.RecordSubagentCompleted(
			taskID,
			status,
			summary,
			costUsd,
			numTurns,
			usage,
			"",
			nil, nil,
		)
		if parentES != nil {
			parentES.EmitSubagentCompleted(taskID, status, summary, toolCount, usage.InputTokens+usage.OutputTokens, time.Since(startedAt).Milliseconds(), costUsd, costKnown, int32(numTurns), "", summary)
		}
	}

	text := result.FinalText()
	if t.outputExtractor != nil {
		if extracted := t.outputExtractor(result); extracted != "" {
			text = extracted
		}
	}
	if text == "" {
		text = "(no output)"
	}
	return ToolResult{Content: text}, nil
}
