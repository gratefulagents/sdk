package agent

import (
	"context"
	"strings"
)

// ImmediateInputPoller injects user messages at runner interruptible boundaries.
// Returned items should usually be user message RunItems (Agent == nil).
type ImmediateInputPoller func(context.Context) ([]RunItem, error)

// CompactionConfig controls proactive context compaction.
type CompactionConfig struct {
	Enabled                     bool
	TriggerTokens               int
	TargetTokens                int
	PreserveRecentItems         int
	PreserveInitialUserMessages int
	SummaryBulletLimit          int
}

// DefaultCompactionConfig returns a conservative default policy for long-running sessions.
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               180000,
		TargetTokens:                100000,
		PreserveRecentItems:         12,
		PreserveInitialUserMessages: 2,
		SummaryBulletLimit:          4,
	}
}

// CompactionDefaultsForModel returns tuned compaction thresholds based on the
// model's context window. Trigger fires when ~10% of context remains (90% used).
// Target compacts down to ~50% of context window.
func CompactionDefaultsForModel(model string) (triggerTokens, targetTokens int) {
	_, unprefixed := ParseModelPrefix(model)
	m := strings.ToLower(strings.TrimSpace(unprefixed))
	switch {
	// Mini models: ~128K context → trigger at 115K, target 64K
	case strings.Contains(m, "mini"):
		return 115000, 64000
	// GPT-5.5 / GPT-5.4: ~1M context → trigger at 900K, target 500K
	case strings.HasPrefix(m, "gpt-5.5"):
		return 900000, 500000
	case strings.HasPrefix(m, "gpt-5.4"):
		return 900000, 500000
	// GPT-5.3-codex: ~1M context → trigger at 900K, target 500K
	case strings.HasPrefix(m, "gpt-5.3-codex"):
		return 900000, 500000
	// GPT-5.2-codex: ~1M context → trigger at 900K, target 500K
	case strings.HasPrefix(m, "gpt-5.2-codex"):
		return 900000, 500000
	// GPT-5.2 / GPT-5.1: ~1M context → trigger at 900K, target 500K
	case strings.HasPrefix(m, "gpt-5.2"), strings.HasPrefix(m, "gpt-5.1"):
		return 900000, 500000
	// Claude Opus 4: 200K context → trigger at 180K, target 100K
	case strings.Contains(m, "opus-4"):
		return 180000, 100000
	// Claude Sonnet 4: 200K context → trigger at 180K, target 100K
	case strings.Contains(m, "sonnet-4"):
		return 180000, 100000
	// Claude Haiku: 200K context → trigger at 180K, target 100K
	case strings.Contains(m, "haiku"):
		return 180000, 100000
	default:
		return 180000, 100000
	}
}

func (c CompactionConfig) normalized() CompactionConfig {
	if c.TriggerTokens <= 0 {
		c.TriggerTokens = 180000
	}
	if c.TargetTokens <= 0 {
		c.TargetTokens = c.TriggerTokens / 2
	}
	if c.TargetTokens >= c.TriggerTokens {
		c.TargetTokens = c.TriggerTokens / 2
	}
	if c.PreserveRecentItems <= 0 {
		c.PreserveRecentItems = 12
	}
	if c.PreserveInitialUserMessages <= 0 {
		c.PreserveInitialUserMessages = 2
	}
	if c.SummaryBulletLimit <= 0 {
		c.SummaryBulletLimit = 4
	}
	return c
}

// HandoffHistoryConfig controls how much history is forwarded across handoffs.
type HandoffHistoryConfig struct {
	Enabled             bool
	MaxTokens           int
	TargetTokens        int
	PreserveRecentItems int
	SummaryBulletLimit  int
}

func (c HandoffHistoryConfig) normalized() HandoffHistoryConfig {
	if c.MaxTokens <= 0 {
		c.MaxTokens = 6000
	}
	if c.TargetTokens <= 0 {
		c.TargetTokens = c.MaxTokens / 2
	}
	if c.TargetTokens >= c.MaxTokens {
		c.TargetTokens = c.MaxTokens / 2
	}
	if c.PreserveRecentItems <= 0 {
		c.PreserveRecentItems = 8
	}
	if c.SummaryBulletLimit <= 0 {
		c.SummaryBulletLimit = 4
	}
	return c
}

// RunConfig configures a single run of the Runner.
type RunConfig struct {
	MaxTurns         int           // default 100
	SubAgentMaxTurns int           // default 50 for nested specialist runs
	Hooks            RunHooks      // run-level lifecycle hooks
	ModelSettings    ModelSettings // override agent's model settings
	ModelOverride    string        // override agent's model name
	TracingDisabled  bool
	TracingProcessor TracingProcessor // export spans to external systems

	// Trace, if set, is used instead of creating a new trace per Run().
	// This allows multiple Run() calls (e.g. across phases) to share a single
	// OTel trace so all spans appear in one waterfall.
	Trace *Trace

	// ParentSpanID is the span under which runner-created spans (generation,
	// function, handoff) are nested. Typically set to the current phase span ID.
	// Falls back to Trace.ID if empty.
	ParentSpanID string

	ImmediateInputPoller      ImmediateInputPoller
	CompactionConfig          CompactionConfig
	CompactionRecorder        func(tokensBefore, tokensAfter int, summary string)
	CompactionFailureReporter func(scope, reason string, tokensBefore, tokensAfter int)
	CompactionCarryForward    func(ctx context.Context) string
	HandoffHistory            HandoffHistoryConfig

	// Tool guardrails — per-tool-call validation (distinct from agent-level guardrails).
	ToolInputGuardrails  []ToolInputGuardrail
	ToolOutputGuardrails []ToolOutputGuardrail

	// WorkDir is the working directory for tool execution (bash cwd, path resolution).
	// Typically set to the cloned repo directory so all tools operate inside the repo.
	WorkDir string

	// ErrorHandler is called when the run encounters an error. If set, the handler
	// decides whether to retry, abort, or continue. If nil, errors abort immediately.
	ErrorHandler RunErrorHandler

	// RetryPolicy optionally retries model-call errors. Provider retry advice
	// and ErrorHandler still run; this policy is an SDK-level fallback.
	RetryPolicy *RetryPolicy

	// AdditionalInstructions are appended to the agent instructions for this run.
	AdditionalInstructions string
	// ModeInstructions is a deprecated alias for AdditionalInstructions.
	// It is retained so operator-era callers keep working.
	ModeInstructions string

	WorkingStateContext    string          // durable working-state summary injected each turn
	ToolAccessLevel        ToolAccessLevel // controls tool access tier (full/read-only)
	Phase                  string          // optional host workflow phase label for hooks/observability
	ToolPolicy             *ToolPolicy     // optional approval and timeout policy applied to tools
	MaxConcurrentSubAgents int             // 0 = unlimited
	ForceFinalSummaryTurn  bool            // reserve the final turn for a no-tool summary instead of hard-failing

	// Debug enables verbose logging (full instructions, tool I/O, conversation items).
	Debug bool

	// UntrustedToolOutputs controls whether tool result content is wrapped with
	// "BEGIN UNTRUSTED TOOL OUTPUT … END UNTRUSTED TOOL OUTPUT" delimiters before
	// it becomes part of the next model turn's input. This mitigates prompt-
	// injection attacks where tool output (e.g. a fetched web page) attempts to
	// override the agent's instructions in subsequent turns.
	//
	// Semantics: nil (zero value) defaults to true (wrap) — secure by default.
	// Set to a pointer to false to opt out (e.g. for trusted in-process tools
	// where wrapping would interfere with downstream formatting). See
	// docs/security.md.
	UntrustedToolOutputs *bool
}

// ShouldTagUntrustedToolOutputs reports whether tool outputs should be tagged
// as untrusted before being fed back to the model. Defaults to true.
func (c *RunConfig) ShouldTagUntrustedToolOutputs() bool {
	if c.UntrustedToolOutputs == nil {
		return true
	}
	return *c.UntrustedToolOutputs
}

// ToolPolicy holds host-supplied approval and timeout settings.
type ToolPolicy struct {
	// ApprovalRequired indicates non-read-only tools need approval.
	ApprovalRequired bool
	// DefaultTimeout in seconds for all tool calls (0 = no timeout).
	DefaultTimeout int
}

// DefaultMaxTurns is used when RunConfig.MaxTurns is 0.
const DefaultMaxTurns = 100

// DefaultSubAgentMaxTurns is used when RunConfig.SubAgentMaxTurns is 0.
const DefaultSubAgentMaxTurns = 50

// EffectiveMaxTurns returns MaxTurns or DefaultMaxTurns if unset.
func (c *RunConfig) EffectiveMaxTurns() int {
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return DefaultMaxTurns
}

// EffectiveSubAgentMaxTurns returns SubAgentMaxTurns or DefaultSubAgentMaxTurns if unset.
func (c *RunConfig) EffectiveSubAgentMaxTurns() int {
	if c.SubAgentMaxTurns > 0 {
		return c.SubAgentMaxTurns
	}
	return DefaultSubAgentMaxTurns
}

// IsReadOnly returns true if the tool access level restricts to read-only tools.
func (c *RunConfig) IsReadOnly() bool {
	return NormalizeToolAccessLevel(c.ToolAccessLevel) == ToolAccessLevelReadOnly
}
