package otel

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"

	gootel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
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
	rootCtx         context.Context // ctx of the most recent root trace span
	traceID         string          // OTel trace ID (hex) of the root trace
	onTraceIDReady  func(string)
	traceIDNotified bool
}

// NewOTelTracingProcessor creates a processor that exports spans via OTLP
// gRPC to the endpoint in $OTEL_EXPORTER_OTLP_ENDPOINT, falling back to
// stdout pretty-print when the variable is unset (dev mode).
func NewOTelTracingProcessor(ctx context.Context, serviceName string) (*OTelTracingProcessor, error) {
	return NewOTelTracingProcessorWithEndpoint(ctx, serviceName, "")
}

// NewOTelTracingProcessorWithEndpoint creates a processor that exports spans
// to an explicit OTLP gRPC endpoint. The endpoint may be schemeless
// ("host:4317") or carry a scheme per the OTel spec ("http://host:4317");
// https selects TLS. When endpoint is empty it falls back to
// $OTEL_EXPORTER_OTLP_ENDPOINT, then to stdout pretty-print.
func NewOTelTracingProcessorWithEndpoint(ctx context.Context, serviceName, endpoint string) (*OTelTracingProcessor, error) {
	var exporter sdktrace.SpanExporter
	var err error

	if strings.TrimSpace(endpoint) == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	hostPort, secure := normalizeOTLPEndpoint(endpoint)
	if hostPort != "" {
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(hostPort)}
		if secure {
			opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(&tls.Config{})))
		} else {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("create OTLP exporter: %w", err)
		}
		log.Printf("OTel tracing: exporting to %s", hostPort)
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

// normalizeOTLPEndpoint converts an OTLP endpoint value into the host:port
// form the gRPC exporter expects. Accepts schemeless "host:4317" as well as
// scheme'd "http://host:4317" / "https://host:4317" (per the OTel env spec);
// an https scheme selects TLS. Any URL path suffix is dropped.
func normalizeOTLPEndpoint(endpoint string) (hostPort string, secure bool) {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return "", false
	}
	if i := strings.Index(e, "://"); i >= 0 {
		secure = strings.EqualFold(e[:i], "https")
		e = e[i+3:]
	}
	if i := strings.IndexByte(e, '/'); i >= 0 {
		e = e[:i]
	}
	return e, secure
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
	o.rootCtx = ctx
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
	// Release every span context retained for parent lookups. rootCtx is kept
	// so any straggler span that starts after the trace ended still joins
	// this trace instead of minting a disconnected one.
	o.ctxs = make(map[string]context.Context)
	o.mu.Unlock()
	if ok {
		span.End()
	}
}

func (o *OTelTracingProcessor) OnSpanStart(s *agent.Span) {
	o.mu.Lock()
	parentCtx, ok := o.ctxs[s.ParentID]
	if !ok {
		// Unknown parent (e.g. a tracker span with no root wired, or a child
		// starting after its trace ended): attach to the most recent trace
		// root so the span stays in the run's waterfall instead of starting
		// a disconnected single-span trace.
		if o.rootCtx != nil {
			parentCtx = o.rootCtx
		} else {
			parentCtx = context.Background()
		}
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
	// Keep o.ctxs[s.ID]: descendants may legitimately start after this span
	// ends (async subagents parented under their spawn tool call). Retained
	// contexts are released when the trace ends.
	o.mu.Unlock()
	if !ok {
		return
	}

	// Span data is typically mutated between start and end (final status,
	// tool output, usage, cost): re-apply the mapped attributes so the
	// exported span carries final values, then add the measured duration.
	_, attrs := mapSpanData(s)
	otelSpan.SetAttributes(attrs...)
	otelSpan.SetAttributes(attribute.Int64("duration_ms", s.DurationMS()))

	if errMsg, isErr := spanErrorStatus(s.Data); isErr {
		otelSpan.SetAttributes(attribute.Bool("error", true))
		otelSpan.SetStatus(codes.Error, truncate(errMsg, 256))
	}

	otelSpan.End()
}

// spanErrorStatus reports whether the span's final data represents a failure
// and returns a human-readable error message for the OTel span status.
func spanErrorStatus(data agent.SpanData) (string, bool) {
	switch d := data.(type) {
	case agent.FunctionSpanData:
		if d.IsError {
			return d.Output, true
		}
	case *agent.FunctionSpanData:
		if d != nil && d.IsError {
			return d.Output, true
		}
	case agent.SubagentSpanData:
		if d.Status == "failed" {
			return d.ResultText, true
		}
	case *agent.SubagentSpanData:
		if d != nil && d.Status == "failed" {
			return d.ResultText, true
		}
	default:
		if gen, ok := spanGenerationData(data); ok && !gen.Success && (gen.Error != "" || gen.Status == "failed") {
			return gen.Error, true
		}
	}
	return "", false
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
			attribute.Int64("subagent.cache_read_tokens", d.CacheReadTokens),
			attribute.Int64("subagent.cache_creation_tokens", d.CacheCreateTokens),
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
		attribute.String("gen.status", d.Status),
		attribute.Bool("gen.usage_available", d.UsageAvailable),
		attribute.Int64("gen.input_tokens", d.PromptTokens),
		attribute.Int64("gen.output_tokens", d.CompletionTokens),
		attribute.Int64("gen.prompt_tokens", d.PromptTokens),
		attribute.Int64("gen.completion_tokens", d.CompletionTokens),
		attribute.Int64("gen.cache_read_tokens", d.CacheReadTokens),
		attribute.Int64("gen.cache_creation_tokens", d.CacheCreateTokens),
		attribute.Bool("gen.input_tokens_include_cache", d.InputTokensIncludeCache),
		attribute.Bool("gen.input_tokens_include_cache_known", d.InputTokensIncludeCacheKnown),
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
