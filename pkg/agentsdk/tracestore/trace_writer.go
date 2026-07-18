package tracestore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/guardrails"
)

// TraceSchemaVersion identifies the shape of persisted trace events. Every
// NDJSON record carries it as "schema_version" together with "run_id".
//
// Identifier semantics:
//   - run_id: the run directory the record belongs to.
//   - turn: the TraceWriter's agent-loop counter, incremented on each
//     OnAgentStart hook. It orders hook events within a run.
//   - generation_turn / attempt_number: the runtime's provider-facing turn
//     and retry attempt, carried on generation span records only. It is a
//     separate counter from "turn".
//   - span_id / parent_id: tracing span identity and parenting.
//   - call_id / parent_call_id: tool invocation identity and the enclosing
//     tool call (for nested/subagent tools).
const TraceSchemaVersion = 2

// CaptureMode controls how much raw content the TraceWriter persists.
type CaptureMode string

const (
	// CaptureMetadata is the default: prompts, reasoning, tool inputs and
	// outputs, and request/response payloads are replaced by content digests
	// (sha256 + byte length). Timing, identifiers, token usage, and status
	// metadata are kept verbatim.
	CaptureMetadata CaptureMode = "metadata"
	// CaptureFull is an explicit high-trust opt-in that persists raw content.
	// Content is still recursively passed through the canonical SDK secret
	// detector plus any operator-provided redactors, but regex redaction is
	// best-effort — full capture must only be enabled where the trace root
	// is trusted to hold sensitive prompt and tool data.
	CaptureFull CaptureMode = "full"
)

// TraceWriterOptions configures capture policy for a TraceWriter.
type TraceWriterOptions struct {
	// Capture selects the capture mode. Empty defaults to CaptureMetadata.
	Capture CaptureMode
	// Redactors are operator-provided detectors applied to every captured
	// string (after the canonical SDK secret detector) in CaptureFull mode.
	Redactors []func(string) string
}

// TraceHealth reports writer-side persistence health so silently dropped or
// truncated records are visible to the host and in run artifacts.
type TraceHealth struct {
	EventsWritten   int64  `json:"events_written"`
	EventsTruncated int64  `json:"events_truncated"`
	EventsDropped   int64  `json:"events_dropped"`
	WriteErrors     int64  `json:"write_errors"`
	LastError       string `json:"last_error,omitempty"`
}

// traceSecretRedactors are key/shape-based JSON redactors applied after
// serialization as a final safety net, in addition to the canonical
// guardrails secret detector applied to structured values before marshaling.
var traceSecretRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|api_key|authorization|password|secret|token)"\s*:\s*")[^"]+(")`),
	regexp.MustCompile(`(?i)(\\"(?:access_token|refresh_token|id_token|api_key|authorization|password|secret|token)\\"\s*:\s*\\")[^\\"]+(\\")`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]+\b`),
}

// TraceWriter implements agent.RunHooks and agent.TracingProcessor to capture
// execution traces into the TraceStore. It records every LLM call, tool
// invocation, handoff, and span in NDJSON files that a proposer agent can
// grep/cat through. By default it captures metadata only; raw prompt/tool
// content requires the explicit CaptureFull opt-in.
type TraceWriter struct {
	store     TraceStore
	runID     string
	turn      atomic.Int32
	capture   CaptureMode
	redactors []func(string) string

	eventsWritten   atomic.Int64
	eventsTruncated atomic.Int64
	eventsDropped   atomic.Int64
	writeErrors     atomic.Int64

	mu             sync.Mutex
	toolStart      map[string]time.Time // callID → start time
	lastErr        string
	loggedCategory map[string]bool // categories whose quota exhaustion was logged
}

// NewTraceWriter creates a TraceWriter with the default metadata-only
// capture policy. Call SetRunID before the run starts.
func NewTraceWriter(store TraceStore) *TraceWriter {
	return NewTraceWriterWithOptions(store, TraceWriterOptions{})
}

// NewTraceWriterWithOptions creates a TraceWriter with an explicit capture
// policy.
func NewTraceWriterWithOptions(store TraceStore, opts TraceWriterOptions) *TraceWriter {
	capture := opts.Capture
	if capture == "" {
		capture = CaptureMetadata
	}
	return &TraceWriter{
		store:          store,
		capture:        capture,
		redactors:      opts.Redactors,
		toolStart:      make(map[string]time.Time),
		loggedCategory: make(map[string]bool),
	}
}

// Health returns a snapshot of writer persistence health.
func (tw *TraceWriter) Health() TraceHealth {
	tw.mu.Lock()
	lastErr := tw.lastErr
	tw.mu.Unlock()
	return TraceHealth{
		EventsWritten:   tw.eventsWritten.Load(),
		EventsTruncated: tw.eventsTruncated.Load(),
		EventsDropped:   tw.eventsDropped.Load(),
		WriteErrors:     tw.writeErrors.Load(),
		LastError:       lastErr,
	}
}

// SetRunID sets the current run ID. Must be called before any hooks fire.
func (tw *TraceWriter) SetRunID(runID string) {
	tw.runID = runID
}

// InitRun creates the run directory and writes initial metadata.
func (tw *TraceWriter) InitRun(meta RunMetadata) error {
	tw.runID = meta.RunID
	_, err := tw.store.CreateRunDir(meta.RunID, meta)
	return err
}

// --- RunHooks implementation ---

func (tw *TraceWriter) OnAgentStart(_ *agent.RunContext, a *agent.Agent) {
	tw.turn.Add(1)
	tw.appendJSON("agent_transitions", map[string]any{
		"type":      "agent_start",
		"agent":     a.Name,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
}

func (tw *TraceWriter) OnAgentEnd(_ *agent.RunContext, a *agent.Agent, output any) {
	tw.appendJSON("agent_transitions", map[string]any{
		"type":      "agent_end",
		"agent":     a.Name,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
}

func (tw *TraceWriter) OnHandoff(_ *agent.RunContext, from *agent.Agent, to *agent.Agent) {
	tw.appendJSON("agent_transitions", map[string]any{
		"type":      "handoff",
		"from":      from.Name,
		"to":        to.Name,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
}

func (tw *TraceWriter) OnToolStart(ctx *agent.RunContext, a *agent.Agent, tool agent.Tool, call agent.ToolCallData) {
	now := time.Now()
	tw.mu.Lock()
	tw.toolStart[call.ID] = now
	tw.mu.Unlock()

	parentCallID := agent.ParentCallIDFromContext(ctx.Context())
	entry := map[string]any{
		"type":      "tool_start",
		"call_id":   call.ID,
		"tool":      tool.Name(),
		"agent":     a.Name,
		"input":     tw.content(json.RawMessage(call.Input)),
		"turn":      tw.turn.Load(),
		"timestamp": now,
	}
	if parentCallID != "" {
		entry["parent_call_id"] = parentCallID
	}
	// Include bash commands as structured trace data for downstream analysis.
	if toolName := tool.Name(); toolName == "Bash" || toolName == "ReadOnlyBash" || toolName == "WorkspaceWriteBash" {
		if cmd := agent.ExtractBashCommand(call.Input); cmd != "" {
			entry["bash_command"] = tw.content(cmd)
		}
	}
	tw.appendJSON("tool_calls", entry)
}

func (tw *TraceWriter) OnToolEnd(ctx *agent.RunContext, a *agent.Agent, tool agent.Tool, call agent.ToolCallData, result agent.ToolResult) {
	now := time.Now()
	tw.mu.Lock()
	var durationMS int64
	if start, ok := tw.toolStart[call.ID]; ok {
		durationMS = now.Sub(start).Milliseconds()
		delete(tw.toolStart, call.ID)
	}
	tw.mu.Unlock()

	parentCallID := agent.ParentCallIDFromContext(ctx.Context())
	entry := map[string]any{
		"type":        "tool_end",
		"call_id":     call.ID,
		"tool":        tool.Name(),
		"agent":       a.Name,
		"output":      tw.content(result.Content),
		"is_error":    result.IsError,
		"duration_ms": durationMS,
		"turn":        tw.turn.Load(),
		"timestamp":   now,
	}
	if parentCallID != "" {
		entry["parent_call_id"] = parentCallID
	}
	tw.appendJSON("tool_calls", entry)
}

func (tw *TraceWriter) OnLLMStart(_ *agent.RunContext, a *agent.Agent) {
	tw.appendJSON("llm_calls", map[string]any{
		"type":      "llm_start",
		"agent":     a.Name,
		"model":     a.Model,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
}

// OnLLMEnd records lifecycle metadata for a completed LLM call. It is
// intentionally metadata-only: the canonical request/response payload for a
// generation is recorded exactly once per attempt by the generation span
// records (generation_start/generation_end in llm_calls), so this hook does
// not mirror texts, reasoning, or response snapshots.
func (tw *TraceWriter) OnLLMEnd(_ *agent.RunContext, a *agent.Agent, response *agent.ModelResponse) {
	if response == nil {
		return
	}

	var textCount, reasoningCount int
	var toolCalls []map[string]any
	for _, item := range response.Items {
		switch item.Type {
		case agent.RunItemMessage:
			if item.Message != nil && item.Message.Text != "" {
				textCount++
			}
		case agent.RunItemReasoning:
			if item.Reasoning != nil && item.Reasoning.Text != "" {
				reasoningCount++
			}
		case agent.RunItemToolCall:
			if item.ToolCall != nil {
				toolCalls = append(toolCalls, map[string]any{
					"id":   item.ToolCall.ID,
					"name": item.ToolCall.Name,
				})
			}
		}
	}

	entry := map[string]any{
		"type":            "llm_end",
		"agent":           a.Name,
		"model":           a.Model,
		"text_count":      textCount,
		"reasoning_count": reasoningCount,
		"tool_calls":      toolCalls,
		"input_tokens":    response.Usage.InputTokens,
		"output_tokens":   response.Usage.OutputTokens,
		"turn":            tw.turn.Load(),
		"timestamp":       time.Now(),
	}
	// Flag whether the provider actually returned usage data.
	entry["usage_populated"] = response.Usage.InputTokens > 0 || response.Usage.OutputTokens > 0
	// Include cache token metrics when available.
	if response.Usage.CacheReadTokens > 0 {
		entry["cache_read_tokens"] = response.Usage.CacheReadTokens
	}
	if response.Usage.CacheCreateTokens > 0 {
		entry["cache_creation_tokens"] = response.Usage.CacheCreateTokens
	}
	tw.appendJSON("llm_calls", entry)
}

// --- TracingProcessor implementation ---

func (tw *TraceWriter) OnTraceStart(trace *agent.Trace) {
	tw.appendJSON("spans", map[string]any{
		"type":       "trace_start",
		"trace_id":   trace.ID,
		"trace_name": trace.Name,
		"timestamp":  trace.StartTime,
	})
}

func (tw *TraceWriter) OnTraceEnd(trace *agent.Trace) {
	tw.appendJSON("spans", map[string]any{
		"type":       "trace_end",
		"trace_id":   trace.ID,
		"trace_name": trace.Name,
		"start_time": trace.StartTime,
		"end_time":   trace.EndTime,
	})
}

func (tw *TraceWriter) OnSpanStart(span *agent.Span) {
	entry := map[string]any{
		"type":      "span_start",
		"span_id":   span.ID,
		"parent_id": span.ParentID,
		"name":      span.Name,
		"timestamp": span.StartTime,
	}
	tw.addSpanData(entry, span.Data)
	tw.appendJSON("spans", entry)
	if d, ok := generationSpanData(span.Data); ok {
		tw.appendGenerationLLMCall("generation_start", span, d)
	}
}

func (tw *TraceWriter) OnSpanEnd(span *agent.Span) {
	entry := map[string]any{
		"type":        "span_end",
		"span_id":     span.ID,
		"parent_id":   span.ParentID,
		"name":        span.Name,
		"start_time":  span.StartTime,
		"end_time":    span.EndTime,
		"duration_ms": span.DurationMS(),
	}
	tw.addSpanData(entry, span.Data)
	tw.appendJSON("spans", entry)
	if d, ok := generationSpanData(span.Data); ok {
		tw.appendGenerationLLMCall("generation_end", span, d)
	}
}

func (tw *TraceWriter) Flush() {
	// FilesystemTraceStore writes are synchronous; nothing to flush.
}

// --- helpers ---

func (tw *TraceWriter) addSpanData(entry map[string]any, data agent.SpanData) {
	if data == nil {
		return
	}
	switch d := data.(type) {
	case agent.GenerationSpanData:
		tw.addGenerationSpanData(entry, d)
	case *agent.GenerationSpanData:
		if d != nil {
			tw.addGenerationSpanData(entry, *d)
		}
	case agent.FunctionSpanData:
		addFunctionSpanData(entry, d)
	case *agent.FunctionSpanData:
		if d != nil {
			addFunctionSpanData(entry, *d)
		}
	case agent.HandoffSpanData:
		addHandoffSpanData(entry, d)
	case *agent.HandoffSpanData:
		if d != nil {
			addHandoffSpanData(entry, *d)
		}
	case agent.GuardrailSpanData:
		addGuardrailSpanData(entry, d)
	case *agent.GuardrailSpanData:
		if d != nil {
			addGuardrailSpanData(entry, *d)
		}
	case agent.CompactionSpanData:
		addCompactionSpanData(entry, d)
	case *agent.CompactionSpanData:
		if d != nil {
			addCompactionSpanData(entry, *d)
		}
	case agent.SessionSpanData:
		addSessionSpanData(entry, d)
	case *agent.SessionSpanData:
		if d != nil {
			addSessionSpanData(entry, *d)
		}
	case agent.SubagentSpanData:
		tw.addSubagentSpanData(entry, d)
	case *agent.SubagentSpanData:
		if d != nil {
			tw.addSubagentSpanData(entry, *d)
		}
	case agent.AgentSpanData:
		addAgentSpanData(entry, d)
	case *agent.AgentSpanData:
		if d != nil {
			addAgentSpanData(entry, *d)
		}
	case agent.RetrySpanData:
		addRetrySpanData(entry, d)
	case *agent.RetrySpanData:
		if d != nil {
			addRetrySpanData(entry, *d)
		}
	}
}

func generationSpanData(data agent.SpanData) (agent.GenerationSpanData, bool) {
	switch d := data.(type) {
	case agent.GenerationSpanData:
		return d, true
	case *agent.GenerationSpanData:
		if d != nil {
			return *d, true
		}
	}
	return agent.GenerationSpanData{}, false
}

func (tw *TraceWriter) appendGenerationLLMCall(eventType string, span *agent.Span, d agent.GenerationSpanData) {
	entry := map[string]any{
		"type":      eventType,
		"span_id":   span.ID,
		"parent_id": span.ParentID,
		"name":      span.Name,
	}
	switch eventType {
	case "generation_start":
		entry["timestamp"] = span.StartTime
	default:
		entry["timestamp"] = span.EndTime
		entry["start_time"] = span.StartTime
		entry["end_time"] = span.EndTime
		entry["duration_ms"] = span.DurationMS()
	}
	tw.addGenerationSpanData(entry, d)
	tw.appendJSON("llm_calls", entry)
	if eventType == "generation_start" && d.Request != nil && d.Request.Instructions != "" {
		tw.writeResolvedInstructionsForAttempt(int(d.Turn), int(d.AttemptNumber), d.Request.Instructions)
	}
}

func (tw *TraceWriter) addGenerationSpanData(entry map[string]any, d agent.GenerationSpanData) {
	entry["span_type"] = "generation"
	model := d.ResolvedModel
	if model == "" {
		model = d.RequestedModel
	}
	if model != "" {
		entry["model"] = model
	}
	entry["requested_model"] = d.RequestedModel
	entry["resolved_model"] = d.ResolvedModel
	entry["model_provider"] = d.ModelProvider
	entry["model_canonical"] = d.ModelCanonical
	entry["attempt_number"] = d.AttemptNumber
	entry["generation_turn"] = d.Turn
	entry["scope"] = d.Scope
	entry["task_id"] = d.TaskID
	entry["status"] = d.Status
	entry["usage_available"] = d.UsageAvailable
	entry["input_tokens"] = d.PromptTokens
	entry["output_tokens"] = d.CompletionTokens
	entry["prompt_tokens"] = d.PromptTokens
	entry["completion_tokens"] = d.CompletionTokens
	entry["cache_read_tokens"] = d.CacheReadTokens
	entry["cache_creation_tokens"] = d.CacheCreateTokens
	entry["input_tokens_include_cache"] = d.InputTokensIncludeCache
	entry["input_tokens_include_cache_known"] = d.InputTokensIncludeCacheKnown
	entry["total_tokens"] = d.TotalTokens
	entry["cost_usd"] = d.CostUSD
	entry["cost_known"] = d.CostKnown
	entry["gen_duration_ms"] = d.LatencyMS
	entry["latency_ms"] = d.LatencyMS
	entry["success"] = d.Success
	entry["retry_scheduled"] = d.RetryScheduled
	entry["retry_after_ms"] = d.RetryAfterMS
	entry["fallback_scheduled"] = d.FallbackScheduled
	entry["fallback_from_model"] = d.FallbackFromModel
	entry["fallback_to_model"] = d.FallbackToModel
	entry["fallback_reason"] = d.FallbackReason
	entry["failure_kind"] = d.FailureKind
	entry["tool_count"] = d.ToolCount
	entry["input_item_count"] = d.InputItemCount
	entry["output_item_count"] = d.OutputItemCount
	entry["instructions_length"] = d.InstructionsLength
	entry["input_token_estimate"] = d.InputTokenEstimate
	entry["request_overhead_token_estimate"] = d.RequestOverheadTokenEstimate
	entry["total_request_token_estimate"] = d.TotalRequestTokenEstimate
	if d.Request != nil {
		entry["request"] = tw.content(d.Request)
	}
	if d.Response != nil {
		entry["response"] = tw.content(d.Response)
	}
	if d.Error != "" {
		entry["error"] = tw.redactText(d.Error)
	}
}

func addFunctionSpanData(entry map[string]any, d agent.FunctionSpanData) {
	entry["span_type"] = "function"
	entry["tool_name"] = d.ToolName
	entry["is_error"] = d.IsError
}

func addHandoffSpanData(entry map[string]any, d agent.HandoffSpanData) {
	entry["span_type"] = "handoff"
	entry["from_agent"] = d.FromAgent
	entry["to_agent"] = d.ToAgent
}

func addGuardrailSpanData(entry map[string]any, d agent.GuardrailSpanData) {
	entry["span_type"] = "guardrail"
	entry["guardrail_name"] = d.GuardrailName
	entry["triggered"] = d.Triggered
}

func addCompactionSpanData(entry map[string]any, d agent.CompactionSpanData) {
	entry["span_type"] = "compaction"
	entry["tokens_before"] = d.TokensBefore
	entry["tokens_after"] = d.TokensAfter
}

func addSessionSpanData(entry map[string]any, d agent.SessionSpanData) {
	entry["span_type"] = "session"
	entry["model"] = d.Model
	entry["cost_usd"] = d.CostUSD
	entry["num_turns"] = d.NumTurns
	entry["input_tokens"] = d.InputTokens
	entry["output_tokens"] = d.OutputTokens
	entry["cache_read_input_tokens"] = d.CacheReadInputTokens
	entry["cache_creation_input_tokens"] = d.CacheCreationInputTokens
	entry["stop_reason"] = d.StopReason
	entry["duration_ms"] = d.DurationMS
}

func (tw *TraceWriter) addSubagentSpanData(entry map[string]any, d agent.SubagentSpanData) {
	entry["span_type"] = "subagent"
	entry["task_id"] = d.TaskID
	entry["subagent_type"] = d.Type
	entry["description"] = tw.redactText(d.Description)
	entry["model"] = d.Model
	entry["status"] = d.Status
	entry["cost_usd"] = d.CostUSD
	entry["num_turns"] = d.NumTurns
	entry["total_tokens"] = d.TotalTokens
	entry["input_tokens"] = d.InputTokens
	entry["output_tokens"] = d.OutputTokens
	entry["cache_read_tokens"] = d.CacheReadTokens
	entry["cache_creation_tokens"] = d.CacheCreateTokens
	entry["tool_count"] = d.ToolCount
	entry["duration_ms"] = d.DurationMS
	entry["stop_reason"] = d.StopReason
	if d.Isolation != "" {
		entry["isolation"] = d.Isolation
	}
	if d.Prompt != "" {
		entry["prompt"] = tw.content(d.Prompt)
	}
	if d.ResultText != "" {
		entry["result_text"] = tw.content(d.ResultText)
	}
	if len(d.FilesRead) > 0 {
		entry["files_read_count"] = len(d.FilesRead)
		entry["files_read"] = tw.content(d.FilesRead)
	}
	if len(d.FilesWritten) > 0 {
		entry["files_written_count"] = len(d.FilesWritten)
		entry["files_written"] = tw.content(d.FilesWritten)
	}
}

func addAgentSpanData(entry map[string]any, d agent.AgentSpanData) {
	entry["span_type"] = "agent"
	entry["agent_name"] = d.AgentName
}

func addRetrySpanData(entry map[string]any, d agent.RetrySpanData) {
	entry["span_type"] = "retry"
	entry["error_code"] = d.ErrorCode
	entry["attempt"] = d.Attempt
}

func (tw *TraceWriter) appendJSON(category string, data map[string]any) {
	if tw.runID == "" {
		return
	}
	data["schema_version"] = TraceSchemaVersion
	data["run_id"] = tw.runID
	b, err := json.Marshal(data)
	if err != nil {
		tw.recordError(fmt.Sprintf("marshal %s entry: %v", category, err))
		log.Printf("[metaharness] failed to marshal %s entry: %v", category, err)
		return
	}
	b = tw.redactJSON(b)
	if len(b) >= defaultMaxTraceEventBytes {
		// Replace the oversized record with an explicit truncation marker so
		// the loss is visible in the artifact instead of silent.
		sum := sha256.Sum256(b)
		stub := map[string]any{
			"schema_version": TraceSchemaVersion,
			"run_id":         tw.runID,
			"type":           "event_truncated",
			"original_type":  data["type"],
			"category":       category,
			"sha256":         hex.EncodeToString(sum[:]),
			"original_bytes": len(b),
			"timestamp":      time.Now(),
		}
		if b, err = json.Marshal(stub); err != nil {
			tw.recordError(fmt.Sprintf("marshal %s truncation marker: %v", category, err))
			return
		}
		tw.eventsTruncated.Add(1)
	}
	if err := tw.store.AppendTrace(tw.runID, category, b); err != nil {
		tw.recordError(fmt.Sprintf("append %s: %v", category, err))
		if errors.Is(err, ErrTraceCategoryFull) || errors.Is(err, ErrTraceEventTooLarge) {
			tw.eventsDropped.Add(1)
			tw.mu.Lock()
			logged := tw.loggedCategory[category]
			tw.loggedCategory[category] = true
			tw.mu.Unlock()
			if !logged {
				log.Printf("[metaharness] dropping %s events: %v", category, err)
			}
			return
		}
		tw.writeErrors.Add(1)
		log.Printf("[metaharness] failed to append %s: %v", category, err)
		return
	}
	tw.eventsWritten.Add(1)
}

func (tw *TraceWriter) recordError(msg string) {
	tw.mu.Lock()
	tw.lastErr = msg
	tw.mu.Unlock()
}

// content applies the capture policy to a content-bearing value. In
// CaptureMetadata mode the value is replaced with a digest descriptor; in
// CaptureFull mode it is recursively redacted before being persisted.
func (tw *TraceWriter) content(v any) any {
	if tw == nil || tw.capture != CaptureFull {
		return contentDigest(v)
	}
	return tw.sanitizeValue(v)
}

// contentDigest describes withheld content: enough to correlate and size the
// payload without persisting it.
func contentDigest(v any) map[string]any {
	b := canonicalContentBytes(v)
	sum := sha256.Sum256(b)
	return map[string]any{
		"captured": false,
		"sha256":   hex.EncodeToString(sum[:]),
		"bytes":    len(b),
	}
}

func canonicalContentBytes(v any) []byte {
	switch t := v.(type) {
	case string:
		return []byte(t)
	case []byte:
		return t
	case json.RawMessage:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return []byte(fmt.Sprintf("%v", v))
		}
		return b
	}
}

// sanitizeValue recursively redacts secrets from structured values before
// they are marshaled, so nested and JSON-escaped credentials are covered by
// the canonical detector rather than only by post-serialization regexes.
func (tw *TraceWriter) sanitizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return tw.redactText(t)
	case json.RawMessage:
		return tw.sanitizeRawJSON([]byte(t))
	case []byte:
		return tw.sanitizeRawJSON(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = tw.sanitizeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = tw.sanitizeValue(val)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, val := range t {
			out[i] = tw.redactText(val)
		}
		return out
	case bool, int, int32, int64, uint, uint32, uint64, float32, float64, time.Time:
		return t
	default:
		// Structs (e.g. request/response snapshots): round-trip through JSON
		// so every nested string field passes through the detectors.
		b, err := json.Marshal(t)
		if err != nil {
			return tw.redactText(fmt.Sprintf("%v", t))
		}
		return tw.sanitizeRawJSON(b)
	}
}

func (tw *TraceWriter) sanitizeRawJSON(b []byte) any {
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return tw.redactText(string(b))
	}
	sanitized := tw.sanitizeValue(decoded)
	out, err := json.Marshal(sanitized)
	if err != nil {
		return tw.redactText(string(b))
	}
	return json.RawMessage(out)
}

// redactText applies the canonical SDK secret detector, the writer's
// key-based JSON redactors, and any operator-provided redactors.
func (tw *TraceWriter) redactText(text string) string {
	out, _, _ := guardrails.RedactSecrets(text)
	for _, redactor := range traceSecretRedactors {
		out = redactor.ReplaceAllString(out, "${1}[REDACTED]${2}")
	}
	if tw != nil {
		for _, redactor := range tw.redactors {
			out = redactor(out)
		}
	}
	return out
}

func (tw *TraceWriter) redactJSON(data []byte) []byte {
	return []byte(tw.redactText(string(data)))
}

// WriteResolvedInstructions saves the resolved system prompt for a given turn.
func (tw *TraceWriter) WriteResolvedInstructions(turn int, instructions string) {
	tw.writeResolvedInstructionsFile(fmt.Sprintf("resolved_instructions/turn_%03d.txt", turn), turn, instructions)
}

func (tw *TraceWriter) writeResolvedInstructionsForAttempt(turn, attempt int, instructions string) {
	tw.writeResolvedInstructionsFile(fmt.Sprintf("resolved_instructions/turn_%03d_attempt_%03d.txt", turn, attempt), turn, instructions)
}

func (tw *TraceWriter) writeResolvedInstructionsFile(relPath string, turn int, instructions string) {
	if tw.runID == "" {
		return
	}
	var payload []byte
	if tw.capture == CaptureFull {
		payload = []byte(tw.redactText(instructions))
	} else {
		// Metadata-only capture: persist a digest descriptor instead of the
		// resolved prompt itself.
		digest, err := json.Marshal(contentDigest(instructions))
		if err != nil {
			tw.recordError(fmt.Sprintf("marshal instructions digest: %v", err))
			return
		}
		payload = digest
	}
	if err := tw.store.WriteFile(tw.runID, relPath, payload); err != nil {
		tw.recordError(fmt.Sprintf("write resolved instructions: %v", err))
		tw.writeErrors.Add(1)
		log.Printf("[metaharness] failed to write resolved instructions for turn %d: %v", turn, err)
	}
}

// FinalizeRun writes a session_end event, persists writer health, and updates
// metadata with FinishedAt.
func (tw *TraceWriter) FinalizeRun(status string) {
	tw.appendJSON("agent_transitions", map[string]any{
		"type":      "session_end",
		"status":    status,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
	tw.writeHealthFile()
	if err := tw.store.UpdateMetadataFinishedAt(tw.runID, time.Now()); err != nil {
		log.Printf("[metaharness] failed to update metadata finished_at: %v", err)
	}
}

// writeHealthFile persists drop/truncation/error counters so trace consumers
// can tell whether the artifact is complete.
func (tw *TraceWriter) writeHealthFile() {
	if tw.runID == "" {
		return
	}
	health := tw.Health()
	data, err := json.MarshalIndent(health, "", "  ")
	if err != nil {
		log.Printf("[metaharness] failed to marshal trace health: %v", err)
		return
	}
	if err := tw.store.WriteFile(tw.runID, "trace_health.json", data); err != nil {
		log.Printf("[metaharness] failed to write trace health: %v", err)
	}
}

// RecordPhaseChange writes a phase_change event to agent_transitions.
func (tw *TraceWriter) RecordPhaseChange(phase string) {
	tw.appendJSON("agent_transitions", map[string]any{
		"type":      "phase_change",
		"phase":     phase,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
}

// RecordModeSwitch writes a mode_switch event and updates metadata.
func (tw *TraceWriter) RecordModeSwitch(fromMode, toMode string) {
	tw.appendJSON("agent_transitions", map[string]any{
		"type":      "mode_switch",
		"from_mode": fromMode,
		"to_mode":   toMode,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
	if err := tw.store.UpdateMetadataMode(tw.runID, toMode); err != nil {
		log.Printf("[metaharness] failed to update metadata mode: %v", err)
	}
}

// WriteMetrics writes aggregated metrics.json for the run.
func (tw *TraceWriter) WriteMetrics(metrics map[string]any) {
	if tw.runID == "" {
		return
	}
	data, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		log.Printf("[metaharness] failed to marshal metrics: %v", err)
		return
	}
	data = tw.redactJSON(data)
	if err := tw.store.WriteFile(tw.runID, "metrics.json", data); err != nil {
		log.Printf("[metaharness] failed to write metrics: %v", err)
	}
}
