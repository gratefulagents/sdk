package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ModelUsageEntry holds per-model token usage.
type ModelUsageEntry struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ProgressEvent is a high-level event stored in the ring buffer.
type ProgressEvent struct {
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"eventType"`
	Summary   string    `json:"summary"`
}

// Note: LogEntry was removed — NDJSON activity logging is fully replaced by
// EventStream (content) + OTel spans (structural observability).

// ProgressSnapshot is a point-in-time copy of the tracker state for host status updates.
type ProgressSnapshot struct {
	CurrentStep              string
	SessionNumber            int32
	LastActivity             string
	AgentCount               int32
	ToolCallCount            int32
	CostUsd                  float64
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	ModelUsage               map[string]ModelUsageEntry
	ApiRetries               int32
	Events                   []ProgressEvent
}

const MaxEvents = 20

// ProgressTracker accumulates state from the native agent loop.
// Metrics (cost, tokens, tool calls) are tracked in memory and exposed
// via Snapshot() for host status updates. Content events go through
// EventStream; structural observability through OTel spans.
type ProgressTracker struct {
	mu                       sync.Mutex
	currentStep              string
	sessionNumber            int32
	lastActivity             string
	agentCount               int32
	toolCallCount            int32
	costUsd                  float64
	inputTokens              int64
	outputTokens             int64
	cacheReadInputTokens     int64
	cacheCreationInputTokens int64
	modelUsage               map[string]ModelUsageEntry
	apiRetries               int32
	events                   []ProgressEvent

	// Subagent tracking: maps task_id to agent metadata.
	taskAgents map[string]AgentMeta

	// lastAssistantText stores the full text from the most recent assistant message.
	lastAssistantText string

	// lastAssistantThinking stores thinking content separately.
	lastAssistantThinking string

	// Per-session counters (reset on SetSession).
	sessionToolCallCount int32
	sessionCostUsd       float64

	// pendingEvents collects session-level events for K8s Event emission.
	pendingEvents []ProgressEvent

	// maxToolResultBytes optionally limits tool result and input_raw content in the event stream.
	// Values <= 0 disable truncation.
	maxToolResultBytes int

	// taskID is set on child trackers — auto-tags all log entries with this task ID.
	taskID string

	// parent points to the parent tracker (for child trackers only).
	parent *ProgressTracker

	// tp emits structural OTel spans for metrics/observability events.
	tp TracingProcessor
	// rootSpanID is the parent span ID for record.go-emitted spans (phase,
	// session, subagent, retry, compaction). Updated by plan.go when the
	// active phase changes.
	rootSpanID string
}

// TrackerOption configures a ProgressTracker.
type TrackerOption func(*ProgressTracker)

// WithMaxToolResultBytes sets the maximum size for tool result/input content in the event stream.
// Values <= 0 disable truncation.
func WithMaxToolResultBytes(n int) TrackerOption {
	return func(t *ProgressTracker) {
		t.maxToolResultBytes = n
	}
}

// WithTracingProcessor sets the OTel tracing processor for emitting structural spans.
func WithTracingProcessor(tp TracingProcessor) TrackerOption {
	return func(t *ProgressTracker) {
		t.tp = tp
	}
}

// NewProgressTracker creates a tracker that aggregates run metrics.
func NewProgressTracker(opts ...TrackerOption) *ProgressTracker {
	t := &ProgressTracker{
		modelUsage:         make(map[string]ModelUsageEntry),
		taskAgents:         make(map[string]AgentMeta),
		maxToolResultBytes: DefaultMaxToolResultBytes,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// NewChildTracker creates a child tracker for a subagent.
// It has its own counters (doesn't affect parent's snapshot).
func NewChildTracker(parent *ProgressTracker, taskID string) *ProgressTracker {
	parent.mu.Lock()
	sessionNum := parent.sessionNumber
	maxBytes := parent.maxToolResultBytes
	rootSpan := parent.rootSpanID
	parent.mu.Unlock()

	return &ProgressTracker{
		sessionNumber:      sessionNum,
		taskID:             taskID,
		parent:             parent,
		modelUsage:         make(map[string]ModelUsageEntry),
		taskAgents:         make(map[string]AgentMeta),
		maxToolResultBytes: maxBytes,
		tp:                 parent.tp,
		rootSpanID:         rootSpan,
	}
}

// SessionNumber returns the current session number.
func (t *ProgressTracker) SessionNumber() int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionNumber
}

// SetTracingProcessor sets the OTel tracing processor for structural span emission.
func (t *ProgressTracker) SetTracingProcessor(tp TracingProcessor) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tp = tp
}

// SetRootSpanID sets the parent span ID for all record.go-emitted spans.
// Call this when the agentrun-level trace is created and update when phases change.
func (t *ProgressTracker) SetRootSpanID(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rootSpanID = id
}

// LastAssistantText returns the full text from the most recent assistant message.
func (t *ProgressTracker) LastAssistantText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastAssistantText
}

// LastAssistantThinking returns the thinking content from the most recent assistant message.
func (t *ProgressTracker) LastAssistantThinking() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastAssistantThinking
}

// CurrentStep returns the currently inferred workflow step.
func (t *ProgressTracker) CurrentStep() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentStep
}

// SetSession sets the session number and initial step.
func (t *ProgressTracker) SetSession(num int32, step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionNumber = num
	t.currentStep = step
	t.sessionToolCallCount = 0
	t.sessionCostUsd = 0
	ev := ProgressEvent{
		Timestamp: time.Now(),
		EventType: "session_start",
		Summary:   fmt.Sprintf("Session %d started: %s", num, step),
	}
	t.addEventLocked(ev)
	t.pendingEvents = append(t.pendingEvents, ev)
}

// SetStep sets the current step.
func (t *ProgressTracker) SetStep(step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.currentStep != step {
		t.currentStep = step
		t.addEventLocked(ProgressEvent{
			Timestamp: time.Now(),
			EventType: "step_change",
			Summary:   fmt.Sprintf("Step: %s", step),
		})
	}
}

// WriteResult records a terminal result summary for compatibility with hosts
// that use the progress tracker as their status source.
func (t *ProgressTracker) WriteResult(status, prURL, errMsg, failedStep string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var parts []string
	if status != "" {
		parts = append(parts, "status="+status)
	}
	if prURL != "" {
		parts = append(parts, "pr="+prURL)
	}
	if failedStep != "" {
		parts = append(parts, "failed_step="+failedStep)
	}
	if errMsg != "" {
		parts = append(parts, "error="+Truncate(errMsg, 120))
	}
	summary := strings.Join(parts, " ")
	if summary == "" {
		summary = "run finished"
	}
	t.lastActivity = summary
	t.addEventLocked(ProgressEvent{
		Timestamp: time.Now(),
		EventType: "run_result",
		Summary:   summary,
	})
}

// Snapshot returns a point-in-time copy of the tracker state.
func (t *ProgressTracker) Snapshot() ProgressSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	events := make([]ProgressEvent, len(t.events))
	copy(events, t.events)
	var mu map[string]ModelUsageEntry
	if len(t.modelUsage) > 0 {
		mu = make(map[string]ModelUsageEntry, len(t.modelUsage))
		for k, v := range t.modelUsage {
			mu[k] = v
		}
	}
	return ProgressSnapshot{
		CurrentStep:              t.currentStep,
		SessionNumber:            t.sessionNumber,
		LastActivity:             t.lastActivity,
		AgentCount:               t.agentCount,
		ToolCallCount:            t.toolCallCount,
		CostUsd:                  t.costUsd,
		InputTokens:              t.inputTokens,
		OutputTokens:             t.outputTokens,
		CacheReadInputTokens:     t.cacheReadInputTokens,
		CacheCreationInputTokens: t.cacheCreationInputTokens,
		ModelUsage:               mu,
		ApiRetries:               t.apiRetries,
		Events:                   events,
	}
}

// DrainPendingEvents returns and clears pending session-level events.
func (t *ProgressTracker) DrainPendingEvents() []ProgressEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	events := make([]ProgressEvent, len(t.pendingEvents))
	copy(events, t.pendingEvents)
	t.pendingEvents = t.pendingEvents[:0]
	return events
}

// addEventLocked appends an event to the ring buffer. Must hold mu.
func (t *ProgressTracker) addEventLocked(ev ProgressEvent) {
	t.events = append(t.events, ev)
	if len(t.events) > MaxEvents {
		t.events = t.events[len(t.events)-MaxEvents:]
	}
}

// updateModelUsageLocked updates per-model token usage. Must hold mu.
func (t *ProgressTracker) inferStepLocked(toolName string, bashCmd string) {
	oldStep := t.currentStep
	switch toolName {
	case "LSP":
		if t.currentStep == "starting" || t.currentStep == "" {
			t.currentStep = "exploring"
		}
	case "Edit", "Write":
		if t.currentStep != "committing" && t.currentStep != "reviewing" {
			t.currentStep = "implementing"
		}
	case "Bash":
		if strings.Contains(bashCmd, "git commit") || strings.Contains(bashCmd, "git add") {
			t.currentStep = "committing"
		} else if strings.Contains(bashCmd, "git diff") {
			if t.currentStep != "implementing" && t.currentStep != "committing" {
				t.currentStep = "reviewing"
			}
		}
	}
	if t.currentStep != oldStep {
		t.addEventLocked(ProgressEvent{
			Timestamp: time.Now(),
			EventType: "step_change",
			Summary:   fmt.Sprintf("Step: %s (via %s)", t.currentStep, toolName),
		})
	}
}

// updateModelUsageLocked accumulates per-model token usage. Must hold mu.
func (t *ProgressTracker) updateModelUsageLocked(model string, input, output, cacheRead, cacheCreation int64) {
	existing := t.modelUsage[model]
	existing.InputTokens += input
	existing.OutputTokens += output
	existing.CacheReadInputTokens += cacheRead
	existing.CacheCreationInputTokens += cacheCreation
	t.modelUsage[model] = existing
}

// ExtractFilePath extracts the file_path from a tool call input (Read, Edit, Write).
func ExtractFilePath(input json.RawMessage) string {
	var v struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(input, &v); err != nil {
		return ""
	}
	return v.FilePath
}

// ExtractPath extracts the path from tool inputs that use a "path" field
// (e.g. read_file, list_files).
func ExtractPath(input json.RawMessage) string {
	var v struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &v); err != nil {
		return ""
	}
	return v.Path
}

// ExtractGrepPattern extracts the pattern from a Grep tool call input.
func ExtractGrepPattern(input json.RawMessage) string {
	var v struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &v); err != nil {
		return ""
	}
	return v.Pattern
}

// ExtractGlobPattern extracts the pattern from a Glob tool call input.
func ExtractGlobPattern(input json.RawMessage) string {
	var v struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &v); err != nil {
		return ""
	}
	return v.Pattern
}

// ExtractAskUserQuestion extracts the question text from an AskUserQuestion tool call.
func ExtractAskUserQuestion(input json.RawMessage) string {
	// Try structured format: {"questions": [{"question": "...", "options": [...]}]}
	var structured struct {
		Questions []struct {
			Question string `json:"question"`
			Header   string `json:"header"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(input, &structured); err == nil && len(structured.Questions) > 0 {
		var parts []string
		for _, q := range structured.Questions {
			text := q.Question
			if len(q.Options) > 0 {
				var opts []string
				for _, o := range q.Options {
					if o.Description != "" {
						opts = append(opts, fmt.Sprintf("%s: %s", o.Label, o.Description))
					} else {
						opts = append(opts, o.Label)
					}
				}
				text += " Options: " + strings.Join(opts, " | ")
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n")
	}

	// Try simple {"question": "..."} format.
	var simple struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(input, &simple); err == nil && simple.Question != "" {
		return simple.Question
	}

	log.Printf("WARN: AskUserQuestion unrecognized format (raw: %s)", string(input))
	return string(input)
}

// AgentMeta holds metadata about a running subagent.
type AgentMeta struct {
	Description  string
	Prompt       string
	SubagentType string
	Model        string
	Isolation    string
}

// ExtractAgentMeta extracts metadata from an Agent tool call input.
func ExtractAgentMeta(input json.RawMessage) AgentMeta {
	var raw struct {
		Description  string `json:"description"`
		Prompt       string `json:"prompt"`
		SubagentType string `json:"subagent_type"`
		Model        string `json:"model"`
		Isolation    string `json:"isolation"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return AgentMeta{Description: "unknown"}
	}
	desc := raw.Description
	if desc == "" {
		desc = Truncate(raw.Prompt, 80)
	}
	return AgentMeta{
		Description:  Truncate(desc, 80),
		Prompt:       raw.Prompt,
		SubagentType: raw.SubagentType,
		Model:        raw.Model,
		Isolation:    raw.Isolation,
	}
}

// ExtractLSPOperation extracts the operation and file path from an LSP tool call.
func ExtractLSPOperation(input json.RawMessage) (string, string) {
	var v struct {
		Operation string `json:"operation"`
		FilePath  string `json:"filePath"`
	}
	json.Unmarshal(input, &v)
	return v.Operation, v.FilePath
}

// ExtractBashCommand extracts the command string from a Bash tool call input.
func ExtractBashCommand(input json.RawMessage) string {
	var bashInput struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &bashInput); err != nil {
		return ""
	}
	return bashInput.Command
}

// TruncateBytes truncates a string to at most n bytes, appending "..." if truncated.
func TruncateBytes(s string, n int) string {
	if n <= 0 {
		return s
	}
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// Truncate trims whitespace, replaces newlines with spaces, and truncates to n runes.
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}

// TruncateMiddle truncates s to at most n runes by keeping the head and tail
// and eliding the middle. Use this for model-facing payloads (sub-agent
// results, command output) where the conclusion at the end of the text is
// usually as important as the beginning; tail-truncation would drop it.
func TruncateMiddle(s string, n int) string {
	const marker = "\n…[elided %d chars]…\n"
	runes := []rune(s)
	if n <= 0 || len(runes) <= n {
		return s
	}
	// Reserve room for the elision marker (estimate generously).
	reserve := len(marker) + 12
	if n <= reserve {
		return string(runes[:n])
	}
	keep := n - reserve
	head := keep * 2 / 3
	tail := keep - head
	elided := len(runes) - head - tail
	return string(runes[:head]) + fmt.Sprintf(marker, elided) + string(runes[len(runes)-tail:])
}
