package otel

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sync"
	"time"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"

	gootel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// OTelTracingProcessor bridges our TracingProcessor interface to the
// OpenTelemetry SDK. It maps Span/agent.SpanData to OTel spans with semantic
// attributes, exporting via any OTLP-compatible backend.
type OTelTracingProcessor struct {
	tracer   oteltrace.Tracer
	provider *sdktrace.TracerProvider

	mu              sync.Mutex
	spans           map[string]oteltrace.Span // our span ID -> OTel span
	ctxs            map[string]context.Context
	traceID         string // OTel trace ID (hex) of the root trace
	onTraceIDReady  func(string)
	traceIDNotified bool
}

// NewOTelTracingProcessor creates a processor that exports spans to stdout.
func NewOTelTracingProcessor(ctx context.Context, serviceName string) (*OTelTracingProcessor, error) {
	return NewOTelTracingProcessorWithEndpoint(ctx, serviceName, "")
}

// NewOTelTracingProcessorWithEndpoint creates a processor that exports spans
// to an explicit OTLP gRPC endpoint. Leave endpoint empty for stdout.
func NewOTelTracingProcessorWithEndpoint(ctx context.Context, serviceName, endpoint string) (*OTelTracingProcessor, error) {
	var exporter sdktrace.SpanExporter
	var err error

	if endpoint != "" {
		exporter, err = otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("create OTLP exporter: %w", err)
		}
		log.Printf("OTel tracing: exporting to %s", endpoint)
	} else {
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("create stdout exporter: %w", err)
		}
		log.Printf("OTel tracing: exporting to stdout")
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(serviceName),
	)

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
	)
	gootel.SetTracerProvider(provider)

	tracer := provider.Tracer("gratefulagents/agent")

	return &OTelTracingProcessor{
		tracer:   tracer,
		provider: provider,
		spans:    make(map[string]oteltrace.Span),
		ctxs:     make(map[string]context.Context),
	}, nil
}

func (o *OTelTracingProcessor) OnTraceStart(trace *agent.Trace) {
	ctx, span := o.tracer.Start(context.Background(), trace.Name,
		oteltrace.WithAttributes(
			attribute.String("trace.id", trace.ID),
		),
	)
	o.mu.Lock()
	o.spans[trace.ID] = span
	o.ctxs[trace.ID] = ctx
	o.traceID = span.SpanContext().TraceID().String()
	cb := o.onTraceIDReady
	shouldNotify := cb != nil && !o.traceIDNotified
	if shouldNotify {
		o.traceIDNotified = true
	}
	tid := o.traceID
	o.mu.Unlock()

	if shouldNotify {
		cb(tid)
	}
}

// TraceID returns the OTel trace ID (hex string) of the root trace.
// Returns empty string if no trace has been started.
func (o *OTelTracingProcessor) TraceID() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.traceID
}

// SetOnTraceIDReady registers a callback fired once when the first trace
// starts and the OTel trace ID becomes available. This allows writing the
// trace ID to external storage (e.g. CRD) early for live trace viewing.
func (o *OTelTracingProcessor) SetOnTraceIDReady(fn func(traceID string)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.onTraceIDReady = fn
}

func (o *OTelTracingProcessor) OnTraceEnd(trace *agent.Trace) {
	o.mu.Lock()
	span, ok := o.spans[trace.ID]
	delete(o.spans, trace.ID)
	delete(o.ctxs, trace.ID)
	o.mu.Unlock()
	if ok {
		span.End()
	}
}

func (o *OTelTracingProcessor) OnSpanStart(s *agent.Span) {
	o.mu.Lock()
	parentCtx, ok := o.ctxs[s.ParentID]
	if !ok {
		parentCtx = context.Background()
	}
	o.mu.Unlock()

	spanName, attrs := mapSpanData(s)
	ctx, otelSpan := o.tracer.Start(parentCtx, spanName,
		oteltrace.WithAttributes(attrs...),
		oteltrace.WithAttributes(
			attribute.String("span.id", s.ID),
			attribute.String("span.parent_id", s.ParentID),
		),
	)

	o.mu.Lock()
	o.spans[s.ID] = otelSpan
	o.ctxs[s.ID] = ctx
	o.mu.Unlock()
}

func (o *OTelTracingProcessor) OnSpanEnd(s *agent.Span) {
	o.mu.Lock()
	otelSpan, ok := o.spans[s.ID]
	delete(o.spans, s.ID)
	delete(o.ctxs, s.ID)
	o.mu.Unlock()
	if !ok {
		return
	}

	// Add duration as an attribute for completed spans.
	otelSpan.SetAttributes(attribute.Int64("duration_ms", s.DurationMS()))

	// Add completion attributes from agent.SpanData.
	if gen, ok := spanGenerationData(s.Data); ok {
		otelSpan.SetAttributes(generationAttributes(gen)...)
	}
	if fn, ok := s.Data.(agent.FunctionSpanData); ok && fn.IsError {
		otelSpan.SetAttributes(attribute.Bool("error", true))
	}
	if fn, ok := s.Data.(*agent.FunctionSpanData); ok && fn != nil && fn.IsError {
		otelSpan.SetAttributes(attribute.Bool("error", true))
	}

	otelSpan.End()
}

// Shutdown flushes pending spans and shuts down the provider.
func (o *OTelTracingProcessor) Shutdown(ctx context.Context) error {
	return o.provider.Shutdown(ctx)
}

func (o *OTelTracingProcessor) Flush() {
	if err := o.provider.ForceFlush(context.Background()); err != nil {
		log.Printf("OTel flush error: %v", err)
	}
}

// mapSpanData converts our agent.SpanData to an OTel span name and attributes.
func mapSpanData(s *agent.Span) (string, []attribute.KeyValue) {
	switch d := s.Data.(type) {
	case agent.AgentSpanData:
		return "agent." + d.AgentName, []attribute.KeyValue{
			attribute.String("agent.name", d.AgentName),
			attribute.String("agent.instructions", redactOTelText(truncate(d.Instructions, 500))),
		}
	case agent.GenerationSpanData:
		return "llm.generation", generationAttributes(d)
	case *agent.GenerationSpanData:
		if d == nil {
			return "llm.generation", nil
		}
		return "llm.generation", generationAttributes(*d)
	case agent.FunctionSpanData:
		return "tool." + d.ToolName, functionAttributes(d)
	case *agent.FunctionSpanData:
		if d == nil {
			return "tool", nil
		}
		return "tool." + d.ToolName, functionAttributes(*d)
	case agent.HandoffSpanData:
		return "handoff", handoffAttributes(d)
	case *agent.HandoffSpanData:
		if d == nil {
			return "handoff", nil
		}
		return "handoff", handoffAttributes(*d)
	case agent.GuardrailSpanData:
		return "guardrail." + d.GuardrailName, []attribute.KeyValue{
			attribute.String("guardrail.name", d.GuardrailName),
			attribute.Bool("guardrail.triggered", d.Triggered),
		}
	case agent.SessionSpanData:
		return "session", []attribute.KeyValue{
			attribute.String("session.model", d.Model),
			attribute.Float64("session.cost_usd", d.CostUSD),
			attribute.Int("session.num_turns", d.NumTurns),
			attribute.Int64("session.duration_ms", d.DurationMS),
			attribute.Int64("session.input_tokens", d.InputTokens),
			attribute.Int64("session.output_tokens", d.OutputTokens),
			attribute.Int64("session.cache_read_tokens", d.CacheReadInputTokens),
			attribute.Int64("session.cache_creation_tokens", d.CacheCreationInputTokens),
			attribute.String("session.stop_reason", d.StopReason),
		}
	case agent.SubagentSpanData:
		attrs := []attribute.KeyValue{
			attribute.String("subagent.task_id", d.TaskID),
			attribute.String("subagent.type", d.Type),
			attribute.String("subagent.description", truncate(d.Description, 200)),
			attribute.String("subagent.model", d.Model),
			attribute.String("subagent.status", d.Status),
			attribute.Float64("subagent.cost_usd", d.CostUSD),
			attribute.Int("subagent.num_turns", d.NumTurns),
			attribute.Int64("subagent.total_tokens", d.TotalTokens),
			attribute.Int64("subagent.input_tokens", d.InputTokens),
			attribute.Int64("subagent.output_tokens", d.OutputTokens),
			attribute.Int("subagent.tool_count", int(d.ToolCount)),
			attribute.Int64("subagent.duration_ms", d.DurationMS),
			attribute.String("subagent.stop_reason", d.StopReason),
		}
		if d.Isolation != "" {
			attrs = append(attrs, attribute.String("subagent.isolation", d.Isolation))
		}
		return "subagent." + d.Type, attrs
	case agent.RetrySpanData:
		return "api.retry", []attribute.KeyValue{
			attribute.String("retry.error_code", d.ErrorCode),
			attribute.Int64("retry.after_ms", d.RetryAfterMS),
			attribute.Int("retry.attempt", int(d.Attempt)),
			attribute.Int("retry.max_retries", int(d.MaxRetries)),
		}
	case agent.CompactionSpanData:
		return "compaction", []attribute.KeyValue{
			attribute.Int("compaction.tokens_before", d.TokensBefore),
			attribute.Int("compaction.tokens_after", d.TokensAfter),
		}
	default:
		return s.Name, nil
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func functionAttributes(d agent.FunctionSpanData) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("tool.name", d.ToolName),
		attribute.String("tool.input", redactOTelText(truncate(d.Input, 1000))),
		attribute.String("tool.output", redactOTelText(truncate(d.Output, 1000))),
		attribute.Bool("tool.error", d.IsError),
	}
}

func handoffAttributes(d agent.HandoffSpanData) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("handoff.from", d.FromAgent),
		attribute.String("handoff.to", d.ToAgent),
	}
}

// otelSecretRedactors strips high-confidence credential material from
// free-text span attributes (instructions, tool input/output) before export,
// mirroring the redaction applied by the filesystem trace store.
var otelSecretRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|api_key|authorization|password|secret|token)"\s*:\s*")[^"]+(")`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]+\b`),
}

func redactOTelText(s string) string {
	for _, r := range otelSecretRedactors {
		if r.NumSubexp() == 2 {
			s = r.ReplaceAllString(s, "${1}[REDACTED]${2}")
		} else {
			s = r.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return s
}

func spanGenerationData(data agent.SpanData) (agent.GenerationSpanData, bool) {
	switch d := data.(type) {
	case agent.GenerationSpanData:
		return d, true
	case *agent.GenerationSpanData:
		if d == nil {
			return agent.GenerationSpanData{}, false
		}
		return *d, true
	default:
		return agent.GenerationSpanData{}, false
	}
}

func generationAttributes(d agent.GenerationSpanData) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen.requested_model", d.RequestedModel),
		attribute.String("gen.resolved_model", d.ResolvedModel),
		attribute.String("gen.model_provider", d.ModelProvider),
		attribute.String("gen.model_canonical", d.ModelCanonical),
		attribute.Int("gen.attempt_number", int(d.AttemptNumber)),
		attribute.Int("gen.turn", int(d.Turn)),
		attribute.String("gen.scope", d.Scope),
		attribute.String("gen.task_id", d.TaskID),
		attribute.String("gen.phase", d.Phase),
		attribute.String("gen.status", d.Status),
		attribute.Bool("gen.usage_available", d.UsageAvailable),
		attribute.Int64("gen.input_tokens", d.PromptTokens),
		attribute.Int64("gen.output_tokens", d.CompletionTokens),
		attribute.Int64("gen.prompt_tokens", d.PromptTokens),
		attribute.Int64("gen.completion_tokens", d.CompletionTokens),
		attribute.Int64("gen.cache_read_tokens", d.CacheReadTokens),
		attribute.Int64("gen.cache_creation_tokens", d.CacheCreateTokens),
		attribute.Int64("gen.total_tokens", d.TotalTokens),
		attribute.Float64("gen.cost_usd", d.CostUSD),
		attribute.Bool("gen.cost_known", d.CostKnown),
		attribute.Int64("gen.latency_ms", d.LatencyMS),
		attribute.Bool("gen.success", d.Success),
		attribute.String("gen.error", d.Error),
		attribute.Bool("gen.retry_scheduled", d.RetryScheduled),
		attribute.Int64("gen.retry_after_ms", d.RetryAfterMS),
		attribute.Bool("gen.fallback_scheduled", d.FallbackScheduled),
		attribute.String("gen.fallback_from_model", d.FallbackFromModel),
		attribute.String("gen.fallback_to_model", d.FallbackToModel),
		attribute.String("gen.fallback_reason", d.FallbackReason),
		attribute.String("gen.failure_kind", d.FailureKind),
		attribute.Int("gen.tool_count", int(d.ToolCount)),
		attribute.Int("gen.input_item_count", int(d.InputItemCount)),
		attribute.Int("gen.output_item_count", int(d.OutputItemCount)),
		attribute.Int("gen.instructions_length", int(d.InstructionsLength)),
	}
}
