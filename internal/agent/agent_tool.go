package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

	runConfig := RunConfig{
		MaxTurns:         DefaultSubAgentMaxTurns,
		SubAgentMaxTurns: DefaultSubAgentMaxTurns,
		WorkDir:          workDir,
		Trace:            TraceFromContext(ctx),
		ParentSpanID:     SpanParentIDFromContext(ctx),
		TracingProcessor: TracingProcessorFromContext(ctx),
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
		// runs cannot bypass approval requirements, tool guardrails,
		// untrusted-output tagging, or output caps that the host configured
		// on the parent run.
		runConfig.ToolPolicy = nestedCfg.ToolPolicy
		runConfig.ToolInputGuardrails = nestedCfg.ToolInputGuardrails
		runConfig.ToolOutputGuardrails = nestedCfg.ToolOutputGuardrails
		runConfig.UntrustedToolOutputs = nestedCfg.UntrustedToolOutputs
		runConfig.MaxToolOutputBytes = nestedCfg.MaxToolOutputBytes
		if NormalizeToolAccessLevel(nestedCfg.ToolAccessLevel) == ToolAccessLevelReadOnly {
			childToolAccess = ToolAccessLevelReadOnly
		}
	}
	runConfig.ToolAccessLevel = childToolAccess

	// Run through the shared sub-agent engine, inheriting the runner's default
	// hooks so sub-agent tool events appear in the parent's activity log.
	spec := subAgentRunSpec{
		Runner:        t.runner,
		Agent:         t.agent,
		Message:       params.Message,
		ParentCallID:  ParentCallIDFromContext(ctx),
		FallbackHooks: t.runner.DefaultHooks,
		RunConfig:     runConfig,
	}
	if platformHooks, ok := t.runner.DefaultHooks.(*PlatformHooks); ok && platformHooks != nil && platformHooks.Tracker != nil {
		spec.TaskID = "task_" + uuid.NewString()
		spec.Tracker = platformHooks.Tracker
		spec.EventStream = platformHooks.EventStream
		spec.Turn = platformHooks.Turn
	}

	outcome := runSubAgentOnce(ctx, spec)
	if outcome.Err != nil {
		return ToolResult{Content: outcome.ErrMsg, IsError: true}, nil
	}

	text := outcome.FinalText
	if t.outputExtractor != nil {
		if extracted := t.outputExtractor(outcome.Result); extracted != "" {
			text = extracted
		}
	}
	if text == "" {
		text = "(no output)"
	}
	return ToolResult{Content: text}, nil
}
