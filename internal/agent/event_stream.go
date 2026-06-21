package agent

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// eventStreamMaxToolBytes caps tool output and input_raw in the event stream
// to keep events.jsonl reasonable for real-time streaming.
const eventStreamMaxToolBytes = 32 * 1024 // 32 KB

// DefaultMaxToolResultBytes is the default cap for tool result/input content.
const DefaultMaxToolResultBytes = eventStreamMaxToolBytes

// ContentEvent is a thin real-time event for host UIs and log sinks.
// Content, status, and key metrics for live display.
// Full structural observability lives in OTel spans.
type ContentEvent struct {
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`
	Session   int32     `json:"session"`

	// Content
	Message string `json:"message,omitempty"`

	// Tool identification and I/O
	Tool           string `json:"tool,omitempty"`
	ToolUseID      string `json:"tool_use_id,omitempty"`
	ParentCallID   string `json:"parent_call_id,omitempty"`
	IsError        bool   `json:"is_error,omitempty"`
	AgentName      string `json:"agent_name,omitempty"`
	InputRaw       string `json:"input_raw,omitempty"`
	Output         string `json:"output,omitempty"`
	ToolDurationMS int64  `json:"tool_duration_ms,omitempty"`

	// Workflow location
	Phase string `json:"phase,omitempty"`

	LLMAttempt int32  `json:"llm_attempt,omitempty"`
	LLMScope   string `json:"llm_scope,omitempty"`

	AttemptNumber       int32  `json:"attempt_number,omitempty"`
	AttemptStatus       string `json:"attempt_status,omitempty"`
	Scope               string `json:"scope,omitempty"`
	RequestedModel      string `json:"requested_model,omitempty"`
	ResolvedModel       string `json:"resolved_model,omitempty"`
	CanonicalModel      string `json:"canonical_model,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Turn                int32  `json:"turn,omitempty"`
	UsageAvailable      bool   `json:"usage_available,omitempty"`
	HasPromptTokens     bool   `json:"has_prompt_tokens,omitempty"`
	HasCompletionTokens bool   `json:"has_completion_tokens,omitempty"`
	HasTotalTokens      bool   `json:"has_total_tokens,omitempty"`
	PromptTokens        int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens    int64  `json:"completion_tokens,omitempty"`
	TotalTokens         int64  `json:"total_tokens,omitempty"`
	AttemptLatencyMs    int64  `json:"attempt_latency_ms,omitempty"`
	RetryPlanned        bool   `json:"retry_planned,omitempty"`
	RetryAfterMs        int64  `json:"retry_after_ms,omitempty"`
	FallbackPlanned     bool   `json:"fallback_planned,omitempty"`
	FallbackFromModel   string `json:"fallback_from_model,omitempty"`
	FallbackToModel     string `json:"fallback_to_model,omitempty"`
	FallbackReason      string `json:"fallback_reason,omitempty"`
	FailureKind         string `json:"failure_kind,omitempty"`

	// Subagent status
	TaskID string `json:"task_id,omitempty"`
	Status string `json:"status,omitempty"`

	// Subagent metadata (populated on subagent_started/subagent_completed)
	SubagentType  string `json:"subagent_type,omitempty"`
	SubagentModel string `json:"subagent_model,omitempty"`

	// Subagent prompt/result (for activity log display)
	SubagentPrompt     string `json:"subagent_prompt,omitempty"`
	SubagentResultText string `json:"subagent_result_text,omitempty"`

	// Subagent completion metrics
	SubagentToolCount  int32   `json:"subagent_tool_count,omitempty"`
	SubagentTokens     int64   `json:"subagent_tokens,omitempty"`
	SubagentDurationMs int64   `json:"subagent_duration_ms,omitempty"`
	SubagentCostUsd    float64 `json:"subagent_cost_usd,omitempty"`
	SubagentCostKnown  bool    `json:"subagent_cost_known,omitempty"`
	SubagentNumTurns   int32   `json:"subagent_num_turns,omitempty"`
	SubagentStopReason string  `json:"subagent_stop_reason,omitempty"`

	// Subagent progress snapshot fields.
	SubagentDependsOn         []string `json:"subagent_depends_on,omitempty"`
	SubagentWaitingOn         []string `json:"subagent_waiting_on,omitempty"`
	SubagentCurrentStep       string   `json:"subagent_current_step,omitempty"`
	SubagentLastTool          string   `json:"subagent_last_tool,omitempty"`
	SubagentFilesWritten      int      `json:"subagent_files_written,omitempty"`
	SubagentMessagesReceived  int      `json:"subagent_messages_received,omitempty"`
	SubagentLastParentMessage string   `json:"subagent_last_parent_message,omitempty"`

	// Step inference (exploring, implementing, committing, etc.)
	Step string `json:"step,omitempty"`

	// Session init metadata (populated on system_init events)
	Model          string   `json:"model,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	Cwd            string   `json:"cwd,omitempty"`
	MaxTurns       int32    `json:"max_turns,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	McpServers     []string `json:"mcp_servers,omitempty"`

	// Session end metrics (populated on session_end events)
	CostUsd                  float64 `json:"cost_usd,omitempty"`
	CostKnown                bool    `json:"cost_known,omitempty"`
	InputTokens              int64   `json:"input_tokens,omitempty"`
	OutputTokens             int64   `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens,omitempty"`
	NumTurns                 int32   `json:"num_turns,omitempty"`
	DurationMs               int64   `json:"duration_ms,omitempty"`
	StopReason               string  `json:"stop_reason,omitempty"`

	// Context compaction metadata.
	TokensBefore int32 `json:"tokens_before,omitempty"`
	TokensAfter  int32 `json:"tokens_after,omitempty"`

	// Hook decision data
	HookName string `json:"hook_name,omitempty"`
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// EventStream writes thin real-time events for UI and log consumers.
// It replaces ProgressTracker's NDJSON LogEntry writing for content events.
// Structural/metrics data is handled by OTel spans instead.
type EventStream struct {
	mu      sync.Mutex
	writeMu *sync.Mutex // shared across parent + children
	w       io.Writer
	logger  *AgentLogger
	events  *contentEventBroadcaster

	sessionNumber int32
	phaseName     string
	currentStep   string
	taskID        string // set on child streams for subagent auto-tagging
}

// NewEventStream creates a stream that writes thin events to w.
func NewEventStream(w io.Writer) *EventStream {
	return &EventStream{
		w:       w,
		writeMu: &sync.Mutex{},
		events:  newContentEventBroadcaster(),
	}
}

// SetLogger attaches a logger that mirrors every event to stdout.
func (es *EventStream) SetLogger(l *AgentLogger) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.logger = l
}

// NewChildEventStream creates a child stream for a subagent.
// Shares the writer and write mutex, auto-tags events with taskID.
func NewChildEventStream(parent *EventStream, taskID string) *EventStream {
	parent.mu.Lock()
	defer parent.mu.Unlock()
	return &EventStream{
		w:             parent.w,
		writeMu:       parent.writeMu,
		logger:        parent.logger,
		events:        parent.events,
		sessionNumber: parent.sessionNumber,
		phaseName:     parent.phaseName,
		currentStep:   parent.currentStep,
		taskID:        taskID,
	}
}

type contentEventBroadcaster struct {
	mu            sync.Mutex
	nextID        int
	subs          map[int]chan ContentEvent
	subagentTypes map[string]string
}

func newContentEventBroadcaster() *contentEventBroadcaster {
	return &contentEventBroadcaster{
		subs:          map[int]chan ContentEvent{},
		subagentTypes: map[string]string{},
	}
}

func (b *contentEventBroadcaster) subscribe(buffer int) (<-chan ContentEvent, func()) {
	if b == nil {
		ch := make(chan ContentEvent)
		close(ch)
		return ch, func() {}
	}
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan ContentEvent, buffer)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			if sub, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(sub)
			}
			b.mu.Unlock()
		})
	}
	return ch, unsubscribe
}

func (b *contentEventBroadcaster) annotateSubagent(ev ContentEvent) ContentEvent {
	if b == nil || ev.Type != "subagent_status" || ev.TaskID == "" {
		return ev
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subagentTypes == nil {
		b.subagentTypes = map[string]string{}
	}
	name := ev.SubagentType
	if name == "" {
		name = ev.AgentName
	}
	if name != "" {
		b.subagentTypes[ev.TaskID] = name
		return ev
	}
	if known := b.subagentTypes[ev.TaskID]; known != "" {
		ev.SubagentType = known
		if ev.AgentName == "" {
			ev.AgentName = known
		}
	}
	return ev
}

func (b *contentEventBroadcaster) broadcast(ev ContentEvent) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		select {
		case sub <- ev:
		default:
		}
	}
}

// Subscribe returns a live in-process stream of content events emitted through
// this EventStream. Child streams share the same subscription bus. The returned
// unsubscribe function closes the channel.
func (es *EventStream) Subscribe(buffer int) (<-chan ContentEvent, func()) {
	if es == nil {
		ch := make(chan ContentEvent)
		close(ch)
		return ch, func() {}
	}
	es.mu.Lock()
	if es.events == nil {
		es.events = newContentEventBroadcaster()
	}
	events := es.events
	es.mu.Unlock()
	return events.subscribe(buffer)
}

// SetSession updates the session number.
func (es *EventStream) SetSession(num int32) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.sessionNumber = num
}

// SetPhase updates the current workflow phase.
func (es *EventStream) SetPhase(name string) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.phaseName = name
}

// SetStep updates the current workflow step.
func (es *EventStream) SetStep(step string) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.currentStep = step
}

// Emit writes a stream event. Auto-tags with session, phase, step, and taskID.
// Also mirrors to stdout via the attached logger.
func (es *EventStream) Emit(ev ContentEvent) {
	es.mu.Lock()
	if ev.Session == 0 {
		ev.Session = es.sessionNumber
	}
	if ev.Phase == "" && es.phaseName != "" {
		ev.Phase = es.phaseName
	}
	if ev.Step == "" && es.currentStep != "" {
		ev.Step = es.currentStep
	}
	if ev.TaskID == "" && es.taskID != "" {
		ev.TaskID = es.taskID
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	l := es.logger
	bus := es.events
	es.mu.Unlock()

	if bus != nil {
		ev = bus.annotateSubagent(ev)
	}

	if l != nil {
		l.Event(ev)
	}
	if bus != nil {
		bus.broadcast(ev)
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	data = append(data, '\n')
	es.writeMu.Lock()
	// Event stream writes are best-effort: if the underlying writer (e.g. a
	// closed pipe to the host) fails there is no recovery path here. Callers
	// who need delivery guarantees should use a buffered/durable writer.
	_, _ = es.w.Write(data)
	es.writeMu.Unlock()
}

// EmitText emits an assistant text event.
func (es *EventStream) EmitText(text string, agentName ...string) {
	ev := ContentEvent{Type: "assistant_text", Message: text}
	if len(agentName) > 0 {
		ev.AgentName = agentName[0]
	}
	es.Emit(ev)
}

// EmitThinking emits an assistant thinking event.
func (es *EventStream) EmitThinking(thinking string, agentName ...string) {
	ev := ContentEvent{Type: "assistant_thinking", Message: thinking}
	if len(agentName) > 0 {
		ev.AgentName = agentName[0]
	}
	es.Emit(ev)
}

// EmitToolStart emits a tool invocation event with optional raw input.
func (es *EventStream) EmitToolStart(toolName, toolUseID, parentCallID, inputSummary, agentName, inputRaw string) {
	es.Emit(ContentEvent{
		Type:         "tool_start",
		Tool:         toolName,
		ToolUseID:    toolUseID,
		ParentCallID: parentCallID,
		Message:      inputSummary,
		AgentName:    agentName,
		InputRaw:     TruncateBytes(inputRaw, eventStreamMaxToolBytes),
	})
}

// EmitToolEnd emits a tool completion event with output and duration.
func (es *EventStream) EmitToolEnd(toolName, toolUseID, parentCallID string, isError bool, agentName, output string, durationMS int64) {
	es.Emit(ContentEvent{
		Type:           "tool_end",
		Tool:           toolName,
		ToolUseID:      toolUseID,
		ParentCallID:   parentCallID,
		IsError:        isError,
		AgentName:      agentName,
		Output:         TruncateBytes(output, eventStreamMaxToolBytes),
		ToolDurationMS: durationMS,
	})
}

// EmitSubagentStarted emits a subagent lifecycle start event with full metadata.
func (es *EventStream) EmitSubagentStarted(taskID, toolUseID, description, subagentType, model, prompt string) {
	es.Emit(ContentEvent{
		Type:           "subagent_status",
		TaskID:         taskID,
		ToolUseID:      toolUseID,
		Status:         "started",
		Message:        description,
		AgentName:      subagentType,
		SubagentType:   subagentType,
		SubagentModel:  model,
		SubagentPrompt: Truncate(prompt, 4096),
	})
}

// EmitSubagentCompleted emits a subagent lifecycle completion event with metrics.
func (es *EventStream) EmitSubagentCompleted(taskID, status, description string, toolCount int32, tokens int64, durationMs int64, costUsd float64, costKnown bool, numTurns int32, stopReason, resultText string) {
	es.Emit(ContentEvent{
		Type:               "subagent_status",
		TaskID:             taskID,
		Status:             status,
		Message:            description,
		SubagentToolCount:  toolCount,
		SubagentTokens:     tokens,
		SubagentDurationMs: durationMs,
		SubagentCostUsd:    costUsd,
		SubagentCostKnown:  costKnown,
		SubagentNumTurns:   numTurns,
		SubagentStopReason: stopReason,
		SubagentResultText: Truncate(resultText, 4096),
	})
}

// EmitSubagentTaskStatus emits a compact subagent status/progress snapshot.
func (es *EventStream) EmitSubagentTaskStatus(task SubAgentTask, message string) {
	ev := subagentTaskContentEvent(task, message)
	es.Emit(ev)
}

func subagentTaskContentEvent(task SubAgentTask, message string) ContentEvent {
	if message == "" {
		message = string(task.Status)
	}
	ev := ContentEvent{
		Type:                      "subagent_status",
		TaskID:                    task.ID,
		Status:                    string(task.Status),
		Message:                   message,
		AgentName:                 task.AgentName,
		SubagentType:              task.AgentName,
		SubagentDependsOn:         append([]string(nil), task.DependsOn...),
		SubagentWaitingOn:         append([]string(nil), task.WaitingOn...),
		SubagentCurrentStep:       task.CurrentStep,
		SubagentLastTool:          task.LastTool,
		SubagentFilesWritten:      task.FilesWritten,
		SubagentMessagesReceived:  task.MessagesReceived,
		SubagentLastParentMessage: task.LastParentMessage,
		SubagentToolCount:         task.ToolCount,
		SubagentTokens:            task.Tokens,
		SubagentDurationMs:        task.Duration.Milliseconds(),
	}
	if task.IsTerminal() {
		ev.SubagentStopReason = string(task.Status)
		ev.SubagentResultText = Truncate(task.Result, 4096)
	}
	return ev
}

// EmitSessionEnd emits a session end marker with final metrics.
func (es *EventStream) EmitSessionEnd(status string, costUsd float64, costKnown bool, inputTokens, outputTokens, cacheRead, cacheCreate int64, numTurns int, durationMs int64, stopReason string) {
	es.Emit(ContentEvent{
		Type:                     "session_end",
		Status:                   status,
		CostUsd:                  costUsd,
		CostKnown:                costKnown,
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheReadInputTokens:     cacheRead,
		CacheCreationInputTokens: cacheCreate,
		NumTurns:                 int32(numTurns),
		DurationMs:               durationMs,
		StopReason:               stopReason,
	})
}

// EmitSystemInit emits a session initialization event with full metadata.
func (es *EventStream) EmitSystemInit(model, permissionMode, cwd string, maxTurns int, toolsList, mcpServers []string) {
	es.Emit(ContentEvent{
		Type:           "system_init",
		Model:          model,
		PermissionMode: permissionMode,
		Cwd:            cwd,
		MaxTurns:       int32(maxTurns),
		Tools:          toolsList,
		McpServers:     mcpServers,
	})
}

func (es *EventStream) EmitCompaction(tokensBefore, tokensAfter int, summary string) {
	es.Emit(ContentEvent{
		Type:         "compact_boundary",
		Message:      "Context compacted",
		Output:       summary,
		TokensBefore: int32(tokensBefore),
		TokensAfter:  int32(tokensAfter),
	})
}

// EmitHookDecision emits a hook/guardrail decision event.
func (es *EventStream) EmitHookDecision(hookName, toolName, decision, reason string) {
	es.Emit(ContentEvent{
		Type:     "hook_decision",
		HookName: hookName,
		Tool:     toolName,
		Decision: decision,
		Reason:   reason,
	})
}

func (es *EventStream) EmitLLMAttempt(args ...interface{}) {
	var ev ContentEvent
	if len(args) == 1 {
		if cev, ok := args[0].(ContentEvent); ok {
			ev = cev
		}
	} else if len(args) == 6 {
		switch attempt := args[0].(type) {
		case int32:
			ev.LLMAttempt = attempt
			ev.AttemptNumber = attempt
		case int:
			ev.LLMAttempt = int32(attempt)
			ev.AttemptNumber = int32(attempt)
		}
		if scope, ok := args[1].(string); ok {
			ev.LLMScope = scope
			ev.Scope = scope
		}
		if agentName, ok := args[2].(string); ok {
			ev.AgentName = agentName
		}
		if model, ok := args[3].(string); ok {
			ev.Model = model
			ev.CanonicalModel = model
		}
		if taskID, ok := args[4].(string); ok {
			ev.TaskID = taskID
		}
		if status, ok := args[5].(string); ok {
			ev.Status = status
			ev.AttemptStatus = status
		}
	}
	ev.Type = "llm_attempt"
	if ev.LLMAttempt == 0 {
		ev.LLMAttempt = ev.AttemptNumber
	}
	if ev.LLMScope == "" {
		ev.LLMScope = ev.Scope
	}
	if ev.Status == "" {
		ev.Status = ev.AttemptStatus
	}
	if ev.Model == "" {
		ev.Model = ev.CanonicalModel
	}
	es.Emit(ev)
}
