package agent

import (
	"context"
	"strings"
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

// WithParentCallID returns a context carrying the given parent tool call ID.
func WithParentCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, parentCallIDKey{}, id)
}

// ParentCallIDFromContext extracts the parent tool call ID, if any.
func ParentCallIDFromContext(ctx context.Context) string {
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
}

func WithNestedRunConfig(ctx context.Context, cfg RunConfig) context.Context {
	return context.WithValue(ctx, nestedRunConfigKey{}, NestedRunConfig{
		MaxTurns:                  cfg.EffectiveSubAgentMaxTurns(),
		CompactionConfig:          cfg.CompactionConfig,
		CompactionRecorder:        cfg.CompactionRecorder,
		CompactionFailureReporter: cfg.CompactionFailureReporter,
		HandoffHistory:            cfg.HandoffHistory,
	})
}

func NestedRunConfigFromContext(ctx context.Context) (NestedRunConfig, bool) {
	cfg, ok := ctx.Value(nestedRunConfigKey{}).(NestedRunConfig)
	return cfg, ok
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
