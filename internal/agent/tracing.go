package agent

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Trace represents a complete execution trace (one Runner.Run invocation).
type Trace struct {
	ID        string
	Name      string
	StartTime time.Time
	EndTime   time.Time
	Spans     []*Span
	mu        sync.Mutex
}

// NewTrace creates a new trace.
func NewTrace(name string) *Trace {
	return &Trace{
		ID:        uuid.NewString(),
		Name:      name,
		StartTime: time.Now(),
	}
}

// AddSpan appends a span to the trace.
func (t *Trace) AddSpan(s *Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Spans = append(t.Spans, s)
}

// Finish marks the trace as complete.
func (t *Trace) Finish() {
	t.EndTime = time.Now()
}

// Span represents a single operation within a trace.
type Span struct {
	ID        string
	ParentID  string
	Name      string
	StartTime time.Time
	EndTime   time.Time
	Data      SpanData
}

// NewSpan creates a new span.
func NewSpan(name string, parentID string, data SpanData) *Span {
	return &Span{
		ID:        uuid.NewString(),
		ParentID:  parentID,
		Name:      name,
		StartTime: time.Now(),
		Data:      data,
	}
}

// Finish marks the span as complete.
func (s *Span) Finish() {
	s.EndTime = time.Now()
}

// DurationMS returns the span duration in milliseconds.
func (s *Span) DurationMS() int64 {
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime).Milliseconds()
	}
	return s.EndTime.Sub(s.StartTime).Milliseconds()
}

// SpanData is a marker interface for span-specific data.
type SpanData interface {
	spanData()
}

// AgentSpanData records data about an agent invocation.
type AgentSpanData struct {
	AgentName    string
	Instructions string
}

func (AgentSpanData) spanData() {}

// GenerationSpanData records data about a model generation.
type GenerationSpanData struct {
	RequestedModel               string
	ResolvedModel                string
	ModelProvider                string
	ModelCanonical               string
	AttemptNumber                int32
	Turn                         int32
	Scope                        string
	TaskID                       string
	Status                       string
	UsageAvailable               bool
	PromptTokens                 int64
	CompletionTokens             int64
	CacheReadTokens              int64
	CacheCreateTokens            int64
	TotalTokens                  int64
	CostUSD                      float64
	CostKnown                    bool
	LatencyMS                    int64
	Success                      bool
	Error                        string
	RetryScheduled               bool
	RetryAfterMS                 int64
	FallbackScheduled            bool
	FallbackFromModel            string
	FallbackToModel              string
	FallbackReason               string
	FailureKind                  string
	ToolCount                    int32
	InputItemCount               int32
	OutputItemCount              int32
	InstructionsLength           int32
	InputTokenEstimate           int32
	RequestOverheadTokenEstimate int32
	TotalRequestTokenEstimate    int32
	Request                      *LLMRequestSnapshot
	Response                     *LLMResponseSnapshot
}

func (GenerationSpanData) spanData() {}

// FunctionSpanData records data about a tool/function execution.
type FunctionSpanData struct {
	ToolName string
	Input    string
	Output   string
	IsError  bool
}

func (FunctionSpanData) spanData() {}

// HandoffSpanData records data about an agent handoff.
type HandoffSpanData struct {
	FromAgent string
	ToAgent   string
}

func (HandoffSpanData) spanData() {}

// GuardrailSpanData records data about a guardrail check.
type GuardrailSpanData struct {
	GuardrailName string
	Triggered     bool
}

func (GuardrailSpanData) spanData() {}

// SessionSpanData records data about a complete session (cost, tokens, duration).
type SessionSpanData struct {
	Model                    string
	CostUSD                  float64
	NumTurns                 int
	DurationMS               int64
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	StopReason               string
}

func (SessionSpanData) spanData() {}

// SubagentSpanData records data about a subagent lifecycle.
type SubagentSpanData struct {
	TaskID       string
	Type         string // role name or type
	Description  string
	Model        string
	Status       string // initializing, running, completed, failed
	CostUSD      float64
	NumTurns     int
	TotalTokens  int64
	InputTokens  int64
	OutputTokens int64
	ToolCount    int32
	DurationMS   int64
	StopReason   string
	Isolation    string
	Prompt       string
	ResultText   string
	FilesRead    []string // files the subagent read
	FilesWritten []string // files the subagent modified
}

func (SubagentSpanData) spanData() {}

// RetrySpanData records data about an API retry.
type RetrySpanData struct {
	ErrorCode    string
	RetryAfterMS int64
	Attempt      int32
	MaxRetries   int32
}

func (RetrySpanData) spanData() {}

// CompactionSpanData records data about context compaction.
type CompactionSpanData struct {
	TokensBefore int
	TokensAfter  int
}

func (CompactionSpanData) spanData() {}

// TracingProcessor receives trace and span lifecycle events.
// Implement this to export traces to external systems.
type TracingProcessor interface {
	OnTraceStart(trace *Trace)
	OnTraceEnd(trace *Trace)
	OnSpanStart(span *Span)
	OnSpanEnd(span *Span)
	Flush()
}

// NoOpTracingProcessor is a TracingProcessor that does nothing.
type NoOpTracingProcessor struct{}

func (NoOpTracingProcessor) OnTraceStart(*Trace) {}
func (NoOpTracingProcessor) OnTraceEnd(*Trace)   {}
func (NoOpTracingProcessor) OnSpanStart(*Span)   {}
func (NoOpTracingProcessor) OnSpanEnd(*Span)     {}
func (NoOpTracingProcessor) Flush()              {}

// MultiTracingProcessor fans out events to multiple processors.
type MultiTracingProcessor struct {
	Processors []TracingProcessor
}

func (m *MultiTracingProcessor) OnTraceStart(t *Trace) {
	for _, p := range m.Processors {
		p.OnTraceStart(t)
	}
}

func (m *MultiTracingProcessor) OnTraceEnd(t *Trace) {
	for _, p := range m.Processors {
		p.OnTraceEnd(t)
	}
}

func (m *MultiTracingProcessor) OnSpanStart(s *Span) {
	for _, p := range m.Processors {
		p.OnSpanStart(s)
	}
}

func (m *MultiTracingProcessor) OnSpanEnd(s *Span) {
	for _, p := range m.Processors {
		p.OnSpanEnd(s)
	}
}

func (m *MultiTracingProcessor) Flush() {
	for _, p := range m.Processors {
		p.Flush()
	}
}
