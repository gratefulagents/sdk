package tracestore

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"
)

var traceSecretRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|api_key|authorization|password|secret|token)"\s*:\s*")[^"]+(")`),
	regexp.MustCompile(`(?i)(\\"(?:access_token|refresh_token|id_token|api_key|authorization|password|secret|token)\\"\s*:\s*\\")[^\\"]+(\\")`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]+\b`),
}

// TraceWriter implements agent.RunHooks and agent.TracingProcessor to capture
// full execution traces into the TraceStore. It records every LLM call, tool
// invocation, handoff, and span in NDJSON files that a proposer agent can
// grep/cat through.
type TraceWriter struct {
	store TraceStore
	runID string
	turn  atomic.Int32

	mu        sync.Mutex
	toolStart map[string]time.Time // callID → start time
}

// NewTraceWriter creates a TraceWriter that writes to the given store.
// Call SetRunID before the run starts.
func NewTraceWriter(store TraceStore) *TraceWriter {
	return &TraceWriter{
		store:     store,
		toolStart: make(map[string]time.Time),
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
		"input":     json.RawMessage(call.Input),
		"turn":      tw.turn.Load(),
		"timestamp": now,
	}
	if parentCallID != "" {
		entry["parent_call_id"] = parentCallID
	}
	// Include bash commands as structured trace data for downstream analysis.
	if toolName := tool.Name(); toolName == "Bash" || toolName == "ReadOnlyBash" || toolName == "WorkspaceWriteBash" {
		if cmd := agent.ExtractBashCommand(call.Input); cmd != "" {
			entry["bash_command"] = cmd
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
		"output":      result.Content,
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

func (tw *TraceWriter) OnLLMEnd(_ *agent.RunContext, a *agent.Agent, response *agent.ModelResponse) {
	if response == nil {
		return
	}

	var texts []string
	var reasoningTexts []string
	var toolCalls []map[string]any
	for _, item := range response.Items {
		switch item.Type {
		case agent.RunItemMessage:
			if item.Message != nil && item.Message.Text != "" {
				texts = append(texts, item.Message.Text)
			}
		case agent.RunItemReasoning:
			if item.Reasoning != nil && item.Reasoning.Text != "" {
				reasoningTexts = append(reasoningTexts, item.Reasoning.Text)
			}
		case agent.RunItemToolCall:
			if item.ToolCall != nil {
				toolCalls = append(toolCalls, map[string]any{
					"id":    item.ToolCall.ID,
					"name":  item.ToolCall.Name,
					"input": json.RawMessage(item.ToolCall.Input),
				})
			}
		}
	}

	entry := map[string]any{
		"type":          "llm_end",
		"agent":         a.Name,
		"model":         a.Model,
		"texts":         texts,
		"tool_calls":    toolCalls,
		"response":      agent.BuildLLMResponseSnapshot(response),
		"input_tokens":  response.Usage.InputTokens,
		"output_tokens": response.Usage.OutputTokens,
		"turn":          tw.turn.Load(),
		"timestamp":     time.Now(),
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
	// Include reasoning/thinking blocks when present.
	if len(reasoningTexts) > 0 {
		entry["reasoning"] = reasoningTexts
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
		addGenerationSpanData(entry, d)
	case *agent.GenerationSpanData:
		if d != nil {
			addGenerationSpanData(entry, *d)
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
		addSubagentSpanData(entry, d)
	case *agent.SubagentSpanData:
		if d != nil {
			addSubagentSpanData(entry, *d)
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
	addGenerationSpanData(entry, d)
	tw.appendJSON("llm_calls", entry)
	if eventType == "generation_start" && d.Request != nil && d.Request.Instructions != "" {
		tw.writeResolvedInstructionsForAttempt(int(d.Turn), int(d.AttemptNumber), d.Request.Instructions)
	}
}

func addGenerationSpanData(entry map[string]any, d agent.GenerationSpanData) {
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
	entry["phase"] = d.Phase
	entry["status"] = d.Status
	entry["usage_available"] = d.UsageAvailable
	entry["input_tokens"] = d.PromptTokens
	entry["output_tokens"] = d.CompletionTokens
	entry["prompt_tokens"] = d.PromptTokens
	entry["completion_tokens"] = d.CompletionTokens
	entry["cache_read_tokens"] = d.CacheReadTokens
	entry["cache_creation_tokens"] = d.CacheCreateTokens
	entry["total_tokens"] = d.TotalTokens
	entry["cost_usd"] = d.CostUSD
	entry["cost_known"] = d.CostKnown
	entry["gen_duration_ms"] = d.LatencyMS
	entry["latency_ms"] = d.LatencyMS
	entry["success"] = d.Success
	entry["retry_scheduled"] = d.RetryScheduled
	entry["retry_after_ms"] = d.RetryAfterMS
	entry["failure_kind"] = d.FailureKind
	entry["tool_count"] = d.ToolCount
	entry["input_item_count"] = d.InputItemCount
	entry["output_item_count"] = d.OutputItemCount
	entry["instructions_length"] = d.InstructionsLength
	entry["input_token_estimate"] = d.InputTokenEstimate
	entry["request_overhead_token_estimate"] = d.RequestOverheadTokenEstimate
	entry["total_request_token_estimate"] = d.TotalRequestTokenEstimate
	if d.Request != nil {
		entry["request"] = d.Request
	}
	if d.Response != nil {
		entry["response"] = d.Response
	}
	if d.Error != "" {
		entry["error"] = d.Error
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

func addSubagentSpanData(entry map[string]any, d agent.SubagentSpanData) {
	entry["span_type"] = "subagent"
	entry["task_id"] = d.TaskID
	entry["subagent_type"] = d.Type
	entry["description"] = d.Description
	entry["model"] = d.Model
	entry["status"] = d.Status
	entry["cost_usd"] = d.CostUSD
	entry["num_turns"] = d.NumTurns
	entry["total_tokens"] = d.TotalTokens
	entry["input_tokens"] = d.InputTokens
	entry["output_tokens"] = d.OutputTokens
	entry["tool_count"] = d.ToolCount
	entry["duration_ms"] = d.DurationMS
	entry["stop_reason"] = d.StopReason
	if d.Isolation != "" {
		entry["isolation"] = d.Isolation
	}
	if d.Prompt != "" {
		entry["prompt"] = d.Prompt
	}
	if d.ResultText != "" {
		entry["result_text"] = d.ResultText
	}
	if len(d.FilesRead) > 0 {
		entry["files_read"] = d.FilesRead
	}
	if len(d.FilesWritten) > 0 {
		entry["files_written"] = d.FilesWritten
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
	b, err := json.Marshal(data)
	if err != nil {
		log.Printf("[metaharness] failed to marshal %s entry: %v", category, err)
		return
	}
	b = redactTraceJSON(b)
	if err := tw.store.AppendTrace(tw.runID, category, b); err != nil {
		log.Printf("[metaharness] failed to append %s: %v", category, err)
	}
}

func redactTraceJSON(data []byte) []byte {
	return []byte(redactTraceText(string(data)))
}

func redactTraceText(text string) string {
	out := text
	for _, redactor := range traceSecretRedactors {
		out = redactor.ReplaceAllString(out, "${1}[REDACTED]${2}")
	}
	return out
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
	if err := tw.store.WriteFile(tw.runID, relPath, []byte(redactTraceText(instructions))); err != nil {
		log.Printf("[metaharness] failed to write resolved instructions for turn %d: %v", turn, err)
	}
}

// FinalizeRun writes a session_end event and updates metadata with FinishedAt.
func (tw *TraceWriter) FinalizeRun(status string) {
	tw.appendJSON("agent_transitions", map[string]any{
		"type":      "session_end",
		"status":    status,
		"turn":      tw.turn.Load(),
		"timestamp": time.Now(),
	})
	if err := tw.store.UpdateMetadataFinishedAt(tw.runID, time.Now()); err != nil {
		log.Printf("[metaharness] failed to update metadata finished_at: %v", err)
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
	data = redactTraceJSON(data)
	if err := tw.store.WriteFile(tw.runID, "metrics.json", data); err != nil {
		log.Printf("[metaharness] failed to write metrics: %v", err)
	}
}
