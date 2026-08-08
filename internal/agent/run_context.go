package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// parentCallIDKey is a context key for threading the parent tool call ID
// into sub-agent runs, enabling nested tool event display.
type parentCallIDKey struct{}

// toolCallIDKey carries the current tool call's ID for hook access.
type toolCallIDKey struct{}

// taskIDKey carries the current subagent task ID for nested runs.
type taskIDKey struct{}

// traceKey carries the active trace for nested sub-agent runs.
type traceKey struct{}

// tracingProcessorKey carries the active tracing processor for nested runs.
type tracingProcessorKey struct{}

// spanParentIDKey carries the current parent span for nested runs.
type spanParentIDKey struct{}

// nestedRunConfigKey carries parent run settings that nested sub-agent tools
// must inherit to keep runtime behavior consistent.
type nestedRunConfigKey struct{}

// parentRunItemsKey carries a snapshot of the completed conversation history
// that precedes the current tool-calling model response. Sub-agent tools may
// opt in to using this history as the initial context for a child run.
type parentRunItemsKey struct{}

// durableIdempotencyKey carries a destination-safe stable key for a tool
// effect. Tools that call a destination supporting idempotency should forward
// this value unchanged.
type durableIdempotencyKey struct{}

// DurableIdempotencyKey derives the same opaque key for every replay of one
// tool call in a durable run.
func DurableIdempotencyKey(runID, toolCallID string) string {
	sum := sha256.Sum256([]byte(runID + ":" + toolCallID))
	return "ga_" + hex.EncodeToString(sum[:])
}

// WithDurableIdempotencyKey attaches a durable external-effect key.
func WithDurableIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, durableIdempotencyKey{}, key)
}

// DurableIdempotencyKeyFromContext returns the key a tool should propagate to
// a destination that offers idempotent or deduplicated requests.
func DurableIdempotencyKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(durableIdempotencyKey{}).(string)
	return value
}

// WithParentCallID returns a context carrying the given parent tool call ID.
func WithParentCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, parentCallIDKey{}, id)
}

// ParentCallIDFromContext extracts the parent tool call ID, if any.
func ParentCallIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(parentCallIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithToolCallID returns a context carrying the current tool call ID.
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallIDFromContext extracts the current tool call ID, if any.
func ToolCallIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(toolCallIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithTaskID returns a context carrying the current subagent task ID.
func WithTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, taskIDKey{}, id)
}

// TaskIDFromContext extracts the current subagent task ID, if any.
func TaskIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(taskIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithTraceContext returns a context carrying trace config for nested runs.
func WithTraceContext(ctx context.Context, trace *Trace, processor TracingProcessor, parentSpanID string) context.Context {
	ctx = context.WithValue(ctx, traceKey{}, trace)
	ctx = context.WithValue(ctx, tracingProcessorKey{}, processor)
	ctx = context.WithValue(ctx, spanParentIDKey{}, parentSpanID)
	return ctx
}

// TraceFromContext extracts the active trace for nested runs.
func TraceFromContext(ctx context.Context) *Trace {
	if v, ok := ctx.Value(traceKey{}).(*Trace); ok {
		return v
	}
	return nil
}

// TracingProcessorFromContext extracts the active tracing processor.
func TracingProcessorFromContext(ctx context.Context) TracingProcessor {
	if v, ok := ctx.Value(tracingProcessorKey{}).(TracingProcessor); ok {
		return v
	}
	return nil
}

// SpanParentIDFromContext extracts the current nested-run parent span ID.
func SpanParentIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(spanParentIDKey{}).(string); ok {
		return v
	}
	return ""
}

type NestedRunConfig struct {
	MaxTurns                  int
	CompactionConfig          CompactionConfig
	CompactionRecorder        func(tokensBefore, tokensAfter int, summary string)
	CompactionFailureReporter func(scope, reason string, tokensBefore, tokensAfter int)
	HandoffHistory            HandoffHistoryConfig
	ToolAccessLevel           ToolAccessLevel
	ToolPolicy                *ToolPolicy
	ToolInputGuardrails       []ToolInputGuardrail
	ToolOutputGuardrails      []ToolOutputGuardrail
	RetryPolicy               *RetryPolicy
	ModelCallTimeout          time.Duration
	UntrustedToolOutputs      *bool
	MaxToolOutputBytes        int
	ToolOutputDir             string
}

func WithNestedRunConfig(ctx context.Context, cfg RunConfig) context.Context {
	return context.WithValue(ctx, nestedRunConfigKey{}, NestedRunConfig{
		MaxTurns:                  cfg.EffectiveSubAgentMaxTurns(),
		CompactionConfig:          cfg.CompactionConfig,
		CompactionRecorder:        cfg.CompactionRecorder,
		CompactionFailureReporter: cfg.CompactionFailureReporter,
		HandoffHistory:            cfg.HandoffHistory,
		ToolAccessLevel:           cfg.ToolAccessLevel,
		ToolPolicy:                cfg.ToolPolicy,
		ToolInputGuardrails:       cfg.ToolInputGuardrails,
		ToolOutputGuardrails:      cfg.ToolOutputGuardrails,
		RetryPolicy:               cfg.RetryPolicy,
		ModelCallTimeout:          cfg.ModelCallTimeout,
		UntrustedToolOutputs:      cfg.UntrustedToolOutputs,
		MaxToolOutputBytes:        cfg.MaxToolOutputBytes,
		ToolOutputDir:             cfg.ToolOutputDir,
	})
}

func NestedRunConfigFromContext(ctx context.Context) (NestedRunConfig, bool) {
	cfg, ok := ctx.Value(nestedRunConfigKey{}).(NestedRunConfig)
	return cfg, ok
}

// WithParentRunItems attaches a defensive copy of the current run history to
// tool execution context. The current assistant tool-call response is omitted
// so inherited history never contains an unmatched tool call.
func WithParentRunItems(ctx context.Context, items []RunItem) context.Context {
	return context.WithValue(ctx, parentRunItemsKey{}, cloneParentRunItems(items))
}

// ParentRunItemsFromContext returns a defensive copy of parent run history.
func ParentRunItemsFromContext(ctx context.Context) []RunItem {
	if ctx == nil {
		return nil
	}
	items, _ := ctx.Value(parentRunItemsKey{}).([]RunItem)
	return cloneParentRunItems(items)
}

func cloneParentRunItems(items []RunItem) []RunItem {
	cloned := make([]RunItem, len(items))
	for i, item := range items {
		cloned[i] = item
		if item.Message != nil {
			value := *item.Message
			value.Images = append([]ImageAttachment(nil), item.Message.Images...)
			cloned[i].Message = &value
		}
		if item.ToolCall != nil {
			value := *item.ToolCall
			value.Input = cloneRaw(item.ToolCall.Input)
			cloned[i].ToolCall = &value
		}
		if item.ToolOutput != nil {
			value := *item.ToolOutput
			cloned[i].ToolOutput = &value
		}
		if item.HandoffCall != nil {
			value := *item.HandoffCall
			cloned[i].HandoffCall = &value
		}
		if item.HandoffOutput != nil {
			value := *item.HandoffOutput
			cloned[i].HandoffOutput = &value
		}
		if item.Reasoning != nil {
			value := *item.Reasoning
			cloned[i].Reasoning = &value
		}
		if item.Compaction != nil {
			value := *item.Compaction
			cloned[i].Compaction = &value
		}
		if item.ToolApproval != nil {
			value := *item.ToolApproval
			value.Input = cloneRaw(item.ToolApproval.Input)
			cloned[i].ToolApproval = &value
		}
	}
	return cloned
}

// ToolAccessLevel controls which tools are available during a run.
type ToolAccessLevel string

const (
	ToolAccessLevelFull     ToolAccessLevel = "full"
	ToolAccessLevelReadOnly ToolAccessLevel = "read-only"
)

// NormalizeToolAccessLevel maps caller/user-facing access strings to the
// runner's two enforcement tiers. Empty means the historical default (full).
// Unknown non-empty values fail closed to read-only so a typo cannot silently
// grant write tools.
func NormalizeToolAccessLevel(level ToolAccessLevel) ToolAccessLevel {
	switch strings.ToLower(strings.TrimSpace(string(level))) {
	case "":
		return ToolAccessLevelFull
	case "full", "write", "workspace-write", "workspace_write", "execution", "danger-full-access", "danger_full_access":
		return ToolAccessLevelFull
	case "read-only", "read_only", "readonly", "analysis":
		return ToolAccessLevelReadOnly
	default:
		return ToolAccessLevelReadOnly
	}
}

// RunContext holds runtime state for a single run.
// RunContext carries per-run SDK state for hooks, tools, tracing, and host metadata.
type RunContext struct {
	ctx       context.Context
	Usage     Usage
	Config    RunConfig
	WorkDir   string
	TaskName  string
	Namespace string
	// Tracing: processor exports spans; trace collects them for the run.
	TracingProcessor TracingProcessor
	Trace            *Trace
	// SpanParentID is the default parent for spans created inside this run
	// (generation, tool, handoff). Hosts may set this to a workflow/span ID.
	SpanParentID string
	// ToolAccessLevel is the tool access tier for this run.
	ToolAccessLevel ToolAccessLevel
}

// newRunContext creates a RunContext for a run.
func newRunContext(ctx context.Context, cfg RunConfig) *RunContext {
	return &RunContext{
		ctx:     ctx,
		Config:  cfg,
		WorkDir: cfg.WorkDir,
	}
}

// Context returns the underlying context.Context.
func (c *RunContext) Context() context.Context { return c.ctx }
