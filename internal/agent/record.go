package agent

import (
	"fmt"
	"time"

	agentpolicy "github.com/gratefulagents/sdk/internal/agent/policy"
)

// Record* methods aggregate metrics (cost, tokens) and emit OTel spans.
// Content events are handled by EventStream (event_stream.go).

// RecordSystemInit logs a system initialization event.
func (t *ProgressTracker) RecordSystemInit(model, cwd string, toolsList []string, maxTurns int, mcpServers []string, permissionMode agentpolicy.PermissionMode) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentStep = "starting"
	t.addEventLocked(ProgressEvent{
		Timestamp: time.Now(),
		EventType: "step_change",
		Summary:   "Agent session initialized",
	})
}

// RecordAssistantText logs assistant text content.
func (t *ProgressTracker) RecordAssistantText(text string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastAssistantText = text
	t.lastActivity = Truncate(text, 120)
}

// RecordAssistantThinking logs assistant thinking content.
func (t *ProgressTracker) RecordAssistantThinking(thinking string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastAssistantThinking = thinking
}

// RecordToolUse logs a tool invocation.
func (t *ProgressTracker) RecordToolUse(toolName, inputSummary, toolUseID string, turn int, rawInput string, agentName string, parentCallID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toolCallCount++
	t.sessionToolCallCount++
	t.lastActivity = fmt.Sprintf("Tool: %s", toolName)
	t.inferStepLocked(toolName, inputSummary)
}

// RecordToolResult logs a tool execution result.
func (t *ProgressTracker) RecordToolResult(toolUseID, toolName, output string, isError bool, durationMS int64, agentName string, parentCallID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	status := "completed"
	if isError {
		status = "failed"
	}
	summary := fmt.Sprintf("Tool %s %s", toolName, status)
	if output != "" {
		summary = fmt.Sprintf("%s: %s", summary, Truncate(output, 120))
	}
	t.lastActivity = summary
	t.addEventLocked(ProgressEvent{
		Timestamp: time.Now(),
		EventType: "tool_result",
		Summary:   summary,
	})
}

// RecordLifecycleEvent logs a normalized lifecycle event.
func (t *ProgressTracker) RecordLifecycleEvent(eventType string, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if eventType == "" {
		eventType = "lifecycle"
	}
	if message != "" {
		t.lastActivity = Truncate(message, 120)
	}
	t.addEventLocked(ProgressEvent{
		Timestamp: time.Now(),
		EventType: eventType,
		Summary:   message,
	})
}

// HookEvent captures the context of a hook invocation for recording.
type HookEvent struct {
	ToolName string
	Type     string
}

// HookResult captures the outcome of a hook invocation for recording.
type HookResult struct {
	Decision string
	Reason   string
}

// RecordHookDecision logs the result of dispatching a pre/post-tool hook.
func (t *ProgressTracker) RecordHookDecision(event HookEvent, result HookResult) {
}

// RecordLLMUsage accumulates cost and token counts from a single LLM call
// so the tracker's Snapshot() stays current between session-complete events.
func (t *ProgressTracker) RecordLLMUsage(model string, costUSD float64, usage Usage) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.costUsd += costUSD
	t.sessionCostUsd += costUSD
	t.inputTokens += usage.InputTokens
	t.outputTokens += usage.OutputTokens
	t.cacheReadInputTokens += usage.CacheReadTokens
	t.cacheCreationInputTokens += usage.CacheCreateTokens
	t.updateModelUsageLocked(model, usage.InputTokens, usage.OutputTokens,
		usage.CacheReadTokens, usage.CacheCreateTokens)
}

// RecordSessionComplete logs session completion with final metrics.
// Note: cost/token accumulation is handled per-call by RecordLLMUsage;
// this method only emits the session_complete event and OTel span.
func (t *ProgressTracker) RecordSessionComplete(model string, costUSD float64, numTurns int, usage Usage, duration time.Duration, stopReason string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	resultEv := ProgressEvent{
		Timestamp: time.Now(),
		EventType: "session_complete",
		Summary:   fmt.Sprintf("Session %d complete: $%.4f, %d turns", t.sessionNumber, costUSD, numTurns),
	}
	t.addEventLocked(resultEv)
	t.pendingEvents = append(t.pendingEvents, resultEv)

	// Emit OTel span for session completion with all metrics.
	if t.tp != nil {
		span := NewSpan("session", t.rootSpanID, SessionSpanData{
			Model:                    model,
			CostUSD:                  costUSD,
			NumTurns:                 numTurns,
			DurationMS:               duration.Milliseconds(),
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheReadInputTokens:     usage.CacheReadTokens,
			CacheCreationInputTokens: usage.CacheCreateTokens,
			StopReason:               stopReason,
		})
		t.tp.OnSpanStart(span)
		span.Finish()
		t.tp.OnSpanEnd(span)
	}
}

// RecordSubagentStarted logs a subagent launch.
func (t *ProgressTracker) RecordSubagentStarted(taskID, toolUseID, description, subagentType, model, isolation, prompt string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.agentCount++
	t.taskAgents[taskID] = AgentMeta{
		SubagentType: subagentType,
		Description:  description,
		Model:        model,
		Isolation:    isolation,
		Prompt:       prompt,
	}
	t.addEventLocked(ProgressEvent{
		Timestamp: time.Now(),
		EventType: "agent_spawn",
		Summary:   fmt.Sprintf("Spawned agent [%s]: %s", subagentType, description),
	})
	// Emit OTel span for subagent start.
	if t.tp != nil {
		span := NewSpan("subagent."+subagentType, t.rootSpanID, SubagentSpanData{
			TaskID:      taskID,
			Type:        subagentType,
			Description: description,
			Model:       model,
			Status:      "initializing",
		})
		t.tp.OnSpanStart(span)
	}
}

// RecordSubagentProgress logs subagent progress (metrics only).
func (t *ProgressTracker) RecordSubagentProgress(taskID string, toolCount int32, totalTokens int64, durationMS int64, lastToolName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	summary := fmt.Sprintf("Subagent %s progress: %d tools, %d tokens", taskID, toolCount, totalTokens)
	if lastToolName != "" {
		summary += fmt.Sprintf(", last tool %s", lastToolName)
	}
	t.lastActivity = summary
	t.addEventLocked(ProgressEvent{
		Timestamp: time.Now(),
		EventType: "subagent_progress",
		Summary:   summary,
	})
}

// RecordSubagentCompleted logs subagent completion.
func (t *ProgressTracker) RecordSubagentCompleted(taskID, status, summary string, costUSD float64, numTurns int, usage Usage, stopReason string, filesRead, filesWritten []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	meta := t.taskAgents[taskID]
	t.lastActivity = Truncate(summary, 120)
	// Accumulate subagent cost and tokens so aggregate metrics stay accurate.
	t.costUsd += costUSD
	t.sessionCostUsd += costUSD
	t.inputTokens += usage.InputTokens
	t.outputTokens += usage.OutputTokens
	// Emit OTel span for subagent completion with full metrics.
	if t.tp != nil {
		span := NewSpan("subagent."+meta.SubagentType, t.rootSpanID, SubagentSpanData{
			TaskID:       taskID,
			Type:         meta.SubagentType,
			Description:  meta.Description,
			Model:        meta.Model,
			Status:       status,
			CostUSD:      costUSD,
			NumTurns:     numTurns,
			TotalTokens:  usage.InputTokens + usage.OutputTokens,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			DurationMS:   0, // filled by span.Finish()
			StopReason:   stopReason,
			Isolation:    meta.Isolation,
			Prompt:       meta.Prompt,
			ResultText:   Truncate(summary, 4096),
			FilesRead:    filesRead,
			FilesWritten: filesWritten,
		})
		t.tp.OnSpanStart(span)
		span.Finish()
		t.tp.OnSpanEnd(span)
	}
	if status == "completed" || status == "failed" || status == "stopped" {
		delete(t.taskAgents, taskID)
	}
}

// RecordAPIRetry logs an API retry event.
func (t *ProgressTracker) RecordAPIRetry(errorCode string, retryAfterMS int64, attempt int32, maxRetries int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.apiRetries++
	if t.tp != nil {
		span := NewSpan("api.retry", t.rootSpanID, RetrySpanData{
			ErrorCode:    errorCode,
			RetryAfterMS: retryAfterMS,
			Attempt:      attempt,
			MaxRetries:   maxRetries,
		})
		t.tp.OnSpanStart(span)
		span.Finish()
		t.tp.OnSpanEnd(span)
	}
}

// RecordCompactBoundary logs a context compaction event with token counts.
func (t *ProgressTracker) RecordCompactBoundary(tokensBefore, tokensAfter int, _ string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tp != nil {
		span := NewSpan("compaction", t.rootSpanID, CompactionSpanData{
			TokensBefore: tokensBefore,
			TokensAfter:  tokensAfter,
		})
		t.tp.OnSpanStart(span)
		span.Finish()
		t.tp.OnSpanEnd(span)
	}
}
