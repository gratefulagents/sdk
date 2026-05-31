package agent

import "sync"

// StreamEventType identifies the kind of stream event.
type StreamEventType int

const (
	StreamEventRawResponse StreamEventType = iota
	StreamEventRunItem
	StreamEventAgentUpdated
	StreamEventContent
	StreamEventSubAgent
)

// StreamEvent is emitted during streamed runs.
type StreamEvent struct {
	Type        StreamEventType
	RawResponse *ModelResponse
	Item        *RunItem
	NewAgent    *Agent
	Content     *ContentEvent
	SubAgent    *SubAgentStreamEvent
	Delta       string
	Name        string // e.g. "tool_call_item.created"
}

// SubAgentStreamEvent is the typed, first-class streamed view of sub-agent
// runtime progress. It is derived from ContentEvent subagent_status events.
type SubAgentStreamEvent struct {
	TaskID            string
	AgentName         string
	Status            string
	Message           string
	ToolUseID         string
	ParentCallID      string
	DependsOn         []string
	WaitingOn         []string
	CurrentStep       string
	LastTool          string
	FilesWritten      int
	MessagesReceived  int
	LastParentMessage string
	ToolCount         int32
	Tokens            int64
	DurationMS        int64
	CostUSD           float64
	CostKnown         bool
	NumTurns          int32
	StopReason        string
	ResultText        string
}

// SubAgentStreamEventFromContentEvent extracts a typed sub-agent event from a
// content event. It returns false for non-subagent events.
func SubAgentStreamEventFromContentEvent(ev ContentEvent) (*SubAgentStreamEvent, bool) {
	if ev.Type != "subagent_status" || ev.TaskID == "" {
		return nil, false
	}
	agentName := ev.AgentName
	if agentName == "" {
		agentName = ev.SubagentType
	}
	return &SubAgentStreamEvent{
		TaskID:            ev.TaskID,
		AgentName:         agentName,
		Status:            ev.Status,
		Message:           ev.Message,
		ToolUseID:         ev.ToolUseID,
		ParentCallID:      ev.ParentCallID,
		DependsOn:         append([]string(nil), ev.SubagentDependsOn...),
		WaitingOn:         append([]string(nil), ev.SubagentWaitingOn...),
		CurrentStep:       ev.SubagentCurrentStep,
		LastTool:          ev.SubagentLastTool,
		FilesWritten:      ev.SubagentFilesWritten,
		MessagesReceived:  ev.SubagentMessagesReceived,
		LastParentMessage: ev.SubagentLastParentMessage,
		ToolCount:         ev.SubagentToolCount,
		Tokens:            ev.SubagentTokens,
		DurationMS:        ev.SubagentDurationMs,
		CostUSD:           ev.SubagentCostUsd,
		CostKnown:         ev.SubagentCostKnown,
		NumTurns:          ev.SubagentNumTurns,
		StopReason:        ev.SubagentStopReason,
		ResultText:        ev.SubagentResultText,
	}, true
}

// StreamedRunResult provides access to streaming events and the final result.
type StreamedRunResult struct {
	Events <-chan StreamEvent
	done   chan *RunResult
	mu     sync.Mutex
	err    error
}

// FinalResult blocks until the run completes and returns the result.
func (s *StreamedRunResult) FinalResult() *RunResult {
	if s == nil {
		return nil
	}
	return <-s.done
}

// Err returns the terminal error from a streamed run, if any. Call it after
// FinalResult or after Events has closed. FinalResult keeps the original
// non-breaking API shape, so this method is the error side channel for
// streamed runs.
func (s *StreamedRunResult) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *StreamedRunResult) setErr(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}
