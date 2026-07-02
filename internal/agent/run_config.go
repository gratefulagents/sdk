package agent

import (
	"context"
	"strings"
	"time"
)

// DefaultModelCallTimeout bounds a single model generation attempt when
// RunConfig.ModelCallTimeout is 0. It is generous enough for slow,
// high-reasoning generations over large contexts, but finite so a hung
// provider connection can never freeze a run indefinitely.
const DefaultModelCallTimeout = 10 * time.Minute

// effectiveModelCallTimeout resolves the per-attempt model-call timeout:
// 0 uses DefaultModelCallTimeout; a negative value disables the timeout.
func effectiveModelCallTimeout(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d == 0 {
		return DefaultModelCallTimeout
	}
	return d
}

// modelCallContext derives a per-attempt context for a single model call so a
// hung provider request cannot freeze the run. The returned cancel func must
// always be called. When the timeout is disabled it returns the parent context
// unchanged with a no-op cancel.
func modelCallContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if to := effectiveModelCallTimeout(timeout); to > 0 {
		return context.WithTimeout(ctx, to)
	}
	return ctx, func() {}
}

// ImmediateInputPoller injects user messages at runner interruptible boundaries.
// Returned items should usually be user message RunItems (Agent == nil).
type ImmediateInputPoller func(context.Context) ([]RunItem, error)

// CompactionModelResolver resolves model-specific compaction thresholds for a
// (possibly provider-prefixed) model name. It returns tuned trigger/target
// token thresholds — typically derived from authoritative provider metadata
// (the /models endpoint), falling back to CompactionDefaultsForModel. ok=false
// means "use the configured defaults unchanged".
//
// The runner consults this once per turn against the model actually being used,
// so a sub-agent running a different model than its parent compacts at its own
// model's context window instead of inheriting the parent's thresholds.
// Implementations must be safe for concurrent use and should cache results.
type CompactionModelResolver func(ctx context.Context, model string) (triggerTokens, targetTokens int, ok bool)

// CompactionConfig controls proactive context compaction.
type CompactionConfig struct {
	Enabled                     bool
	TriggerTokens               int
	TargetTokens                int
	PreserveRecentItems         int
	PreserveInitialUserMessages int
	SummaryBulletLimit          int
	// UseLLMSummary asks the active model to write the compaction summary
	// (findings, decisions, state, pending work) instead of the mechanical
	// tool/file digest. Falls back to the deterministic summary whenever the
	// model call fails or produces an unusable result.
	UseLLMSummary bool
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
		UseLLMSummary:               true,
	}
}

// CompactionDefaultsForModel returns tuned compaction thresholds based on the
// model's context window. Trigger fires when ~10% of context remains (90% used).
// Target compacts down to ~50% of context window.
//
// This is the STATIC fallback used only when authoritative provider metadata
// (the /models endpoint) is unavailable for the model. It deliberately errs
// toward SMALLER windows for unknown/small variants: over-compaction merely
// wastes a little context, but under-compaction lets a run blow past the real
// limit and hang (see the gpt-5.3-codex-spark freeze). Small fast variants
// (-spark/-nano/-mini/-lite/-flash) are therefore capped conservatively even
// though some of them advertise larger windows; provider metadata overrides
// these when present.
func CompactionDefaultsForModel(model string) (triggerTokens, targetTokens int) {
	_, unprefixed := ParseModelPrefix(model)
	m := strings.ToLower(strings.TrimSpace(unprefixed))
	switch {
	// Small/fast variants (e.g. gpt-5.3-codex-spark, *-nano, *-mini, *-lite,
	// *-flash): assume a ~128K context and compact conservatively. Checked
	// FIRST so e.g. "gpt-5.3-codex-spark" does not fall through to the
	// "gpt-5.3-codex" 1M branch below.
	case strings.Contains(m, "spark"),
		strings.Contains(m, "nano"),
		strings.Contains(m, "mini"),
		strings.Contains(m, "lite"),
		strings.Contains(m, "flash"):
		return 110000, 60000
	// GPT-5.x / codex (non-spark) on Copilot/OpenAI are ~400K context (not the
	// ~1M once assumed): trigger ~360K (90%), target ~200K (50%). Providers
	// that genuinely expose ~1M are corrected upward by provider metadata.
	case strings.HasPrefix(m, "gpt-5.5"):
		return 360000, 200000
	case strings.HasPrefix(m, "gpt-5.4"):
		return 360000, 200000
	case strings.HasPrefix(m, "gpt-5.3-codex"):
		return 360000, 200000
	case strings.HasPrefix(m, "gpt-5.2-codex"):
		return 360000, 200000
	case strings.HasPrefix(m, "gpt-5.2"), strings.HasPrefix(m, "gpt-5.1"):
		return 360000, 200000
	// Claude Fable 5: 1M context on Copilot and Anthropic (models.dev).
	// Provider /models limits under-report this deployment (claims a 200K
	// prompt cap while 1M-context requests succeed), so the models.dev
	// resolver is authoritative; this static fallback matches it.
	case strings.Contains(m, "fable"):
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
	FallbackModels   []string      // ordered cross-provider fallback models for failed model calls
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
	CompactionModelResolver   CompactionModelResolver
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

	WorkingStateContext    string          // durable working-state summary appended to compaction carry-forward
	ToolAccessLevel        ToolAccessLevel // controls tool access tier (full/read-only)
	ToolPolicy             *ToolPolicy     // optional approval and timeout policy applied to tools
	MaxConcurrentSubAgents int             // 0 = unlimited
	ForceFinalSummaryTurn  bool            // reserve the final turn for a no-tool summary instead of hard-failing

	// RequireCompletionConfirmation bounces the model's first final answer
	// back with a verification prompt; only a second consecutive final answer
	// ends the run. Any tool call or handoff in between resets the
	// confirmation. Prevents premature completion on long autonomous tasks
	// (Terminus 2 double-confirm pattern).
	RequireCompletionConfirmation bool

	// PlanRecitation, when set, is invoked before every model request and its
	// non-empty return value is appended as the final input message of that
	// request only — it is never persisted to run history. Reciting the
	// current plan/goals at the context tail keeps them in the model's
	// high-attention window (the todo.md recitation pattern) while leaving
	// the cache-stable instruction prefix and append-only history untouched.
	PlanRecitation func(ctx context.Context) string

	// ConsecutiveToolErrorLimit escalates when this many consecutive tool
	// turns produce only errors: the runner injects a corrective note telling
	// the model to change approach or report the blocker (the 12-factor
	// three-strike rule). 0 uses DefaultConsecutiveToolErrorLimit; negative
	// disables escalation.
	ConsecutiveToolErrorLimit int

	// ReadOnlyStallTurnLimit nudges when this many consecutive tool turns use
	// only read-only tools (exploration without any state-changing action or
	// final answer). Deep-reasoning models legitimately explore for several
	// turns, so the limit is generous; the nudge asks the model to converge —
	// act on what it knows or deliver its answer — instead of re-reading.
	// 0 uses DefaultReadOnlyStallTurnLimit; negative disables the nudge.
	ReadOnlyStallTurnLimit int

	// StopGate is a deterministic finalization gate. When set, it runs on
	// every candidate final answer; returning ok=false blocks finalization
	// and feeds the feedback back to the model. After StopGateMaxBlocks
	// consecutive blocks (default DefaultStopGateMaxBlocks) the gate is
	// bypassed so a broken gate cannot loop forever (Claude Code stop-hook
	// pattern). Consecutive-block tracking resets when the model makes
	// progress with tools.
	StopGate func(ctx context.Context, finalText string) (ok bool, feedback string)
	// StopGateMaxBlocks caps consecutive StopGate blocks. 0 = default.
	StopGateMaxBlocks int

	// FinalAnswerVerifier, when set, reviews the candidate final answer once
	// per run (after StopGate passes). Non-empty feedback is injected and the
	// run continues instead of finalizing — typically wired to a read-only
	// critic sub-agent that tries to refute the result (adversarial
	// verification). Errors from the verifier are logged and ignored.
	FinalAnswerVerifier func(ctx context.Context, finalText string) (feedback string, err error)

	// MaxToolOutputBytes caps the tool result content fed back to the model as
	// next-turn input. Oversized outputs are middle-truncated (head and tail
	// preserved) so conclusions survive. Hooks, traces, and the event stream
	// still receive the raw output. 0 uses DefaultMaxToolOutputBytes; negative
	// disables the cap.
	MaxToolOutputBytes int

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

	// ModelCallTimeout bounds a single model generation attempt (including the
	// provider-native compaction call). A hung or unresponsive provider request
	// is cancelled after this duration so the attempt fails (and is then
	// retried/fallen-back per policy) instead of freezing the run forever — the
	// failure mode that previously required a kill -9. The timeout applies
	// per-attempt, so retries each get a fresh budget. 0 uses
	// DefaultModelCallTimeout; negative disables the per-call timeout.
	ModelCallTimeout time.Duration
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

// DefaultMaxToolOutputBytes caps model-facing tool output when
// RunConfig.MaxToolOutputBytes is 0. 64 KB (~16K tokens) bounds the context
// cost of a single pathological tool call while staying generous enough for
// file reads and command output (codex-cli truncates exec output at ~10K
// tokens at history-record time for the same reason).
const DefaultMaxToolOutputBytes = 64 * 1024

// DefaultConsecutiveToolErrorLimit is the consecutive all-error tool-turn
// threshold that triggers a corrective escalation note.
const DefaultConsecutiveToolErrorLimit = 3

// DefaultReadOnlyStallTurnLimit is the consecutive read-only tool-turn
// threshold that triggers a converge nudge. Ten turns gives reasoning-heavy
// models room for legitimate exploration before being asked to commit.
const DefaultReadOnlyStallTurnLimit = 10

// maxReadOnlyStallNudgesPerRun caps converge nudges per run so a genuinely
// research-only task is not nagged forever.
const maxReadOnlyStallNudgesPerRun = 3

// DefaultStopGateMaxBlocks caps consecutive StopGate blocks before the gate
// is bypassed.
const DefaultStopGateMaxBlocks = 8

// EffectiveConsecutiveToolErrorLimit returns the escalation threshold, or 0
// when escalation is disabled.
func (c *RunConfig) EffectiveConsecutiveToolErrorLimit() int {
	if c.ConsecutiveToolErrorLimit < 0 {
		return 0
	}
	if c.ConsecutiveToolErrorLimit == 0 {
		return DefaultConsecutiveToolErrorLimit
	}
	return c.ConsecutiveToolErrorLimit
}

// EffectiveReadOnlyStallTurnLimit returns the converge-nudge threshold, or 0
// when the nudge is disabled.
func (c *RunConfig) EffectiveReadOnlyStallTurnLimit() int {
	if c.ReadOnlyStallTurnLimit < 0 {
		return 0
	}
	if c.ReadOnlyStallTurnLimit == 0 {
		return DefaultReadOnlyStallTurnLimit
	}
	return c.ReadOnlyStallTurnLimit
}

// EffectiveStopGateMaxBlocks returns the consecutive-block cap for StopGate.
func (c *RunConfig) EffectiveStopGateMaxBlocks() int {
	if c.StopGateMaxBlocks > 0 {
		return c.StopGateMaxBlocks
	}
	return DefaultStopGateMaxBlocks
}

// EffectiveMaxTurns returns MaxTurns or DefaultMaxTurns if unset.
func (c *RunConfig) EffectiveMaxTurns() int {
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return DefaultMaxTurns
}

// EffectiveMaxToolOutputBytes returns the model-facing tool output cap.
// Returns 0 when the cap is disabled (negative configuration).
func (c *RunConfig) EffectiveMaxToolOutputBytes() int {
	if c.MaxToolOutputBytes < 0 {
		return 0
	}
	if c.MaxToolOutputBytes == 0 {
		return DefaultMaxToolOutputBytes
	}
	return c.MaxToolOutputBytes
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
