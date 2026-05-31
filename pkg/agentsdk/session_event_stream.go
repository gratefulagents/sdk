package agentsdk

import (
	"encoding/json"
	"io"
	"strings"
)

// SessionEventStreamOptions configures NewSessionEventStream.
type SessionEventStreamOptions struct {
	Logger       *AgentLogger
	EnableLogger bool
	LogLevel     LogLevel
	Session      int32
	Phase        string

	SystemInit *SessionSystemInit
}

type SessionSystemInit struct {
	Model          string
	PermissionMode string
	Cwd            string
	MaxTurns       int
	Tools          []string
	MCPServers     []string
}

// NewSessionEventStream creates an EventStream with common session metadata
// already wired and optionally emits system_init.
func NewSessionEventStream(w io.Writer, opts SessionEventStreamOptions) *EventStream {
	es := NewEventStream(w)
	if opts.Logger != nil {
		es.SetLogger(opts.Logger)
	} else if opts.EnableLogger {
		es.SetLogger(NewAgentLogger(opts.LogLevel))
	}
	if opts.Session != 0 {
		es.SetSession(opts.Session)
	}
	if opts.Phase != "" {
		es.SetPhase(opts.Phase)
	}
	if opts.SystemInit != nil {
		es.EmitSystemInit(
			opts.SystemInit.Model,
			opts.SystemInit.PermissionMode,
			opts.SystemInit.Cwd,
			opts.SystemInit.MaxTurns,
			opts.SystemInit.Tools,
			opts.SystemInit.MCPServers,
		)
	}
	return es
}

// ChildToolEvent is the typed SDK view of a child/sub-agent tool event emitted
// through the session event stream.
type ChildToolEvent struct {
	ParentCallID string
	CallID       string
	AgentName    string
	Tool         string
	Phase        string
	InputRaw     string
	Output       string
	IsError      bool
	DurationMS   int64
}

// ParseContentEventLine parses one JSONL content-event line.
func ParseContentEventLine(line string) (ContentEvent, bool) {
	if strings.TrimSpace(line) == "" {
		return ContentEvent{}, false
	}
	var ev ContentEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ContentEvent{}, false
	}
	if ev.Type == "" {
		return ContentEvent{}, false
	}
	return ev, true
}

// ChildToolEventFromContentEvent extracts child tool start/end data from a
// session event. The event is considered a child tool event when it carries a
// parent call id.
func ChildToolEventFromContentEvent(ev ContentEvent) (*ChildToolEvent, bool) {
	if ev.ParentCallID == "" {
		return nil, false
	}
	switch ev.Type {
	case "tool_start":
		return &ChildToolEvent{
			ParentCallID: ev.ParentCallID,
			CallID:       ev.ToolUseID,
			AgentName:    ev.AgentName,
			Tool:         ev.Tool,
			Phase:        "start",
			InputRaw:     ev.InputRaw,
		}, true
	case "tool_end":
		return &ChildToolEvent{
			ParentCallID: ev.ParentCallID,
			CallID:       ev.ToolUseID,
			AgentName:    ev.AgentName,
			Tool:         ev.Tool,
			Phase:        "end",
			Output:       ev.Output,
			IsError:      ev.IsError,
			DurationMS:   ev.ToolDurationMS,
		}, true
	default:
		return nil, false
	}
}

// CompactContentEvent returns a short, human-readable rendering of a content
// event for debug logs.
func CompactContentEvent(ev ContentEvent) string {
	parts := []string{ev.Type}
	if ev.AgentName != "" {
		parts = append(parts, "agent="+ev.AgentName)
	}
	if ev.Tool != "" {
		parts = append(parts, "tool="+ev.Tool)
	}
	if ev.Status != "" {
		parts = append(parts, "status="+ev.Status)
	}
	if ev.IsError {
		parts = append(parts, "error=true")
	}
	if ev.FailureKind != "" {
		parts = append(parts, "failure="+ev.FailureKind)
	}
	if ev.Reason != "" {
		parts = append(parts, firstContentEventLine(ev.Reason))
	}
	if ev.Message != "" {
		parts = append(parts, firstContentEventLine(ev.Message))
	}
	if ev.IsError && ev.Output != "" {
		parts = append(parts, firstContentEventLine(ev.Output))
	}
	return strings.Join(parts, " ")
}

// CompactContentEventLine parses and compacts a JSONL event line. Non-event
// lines fall back to their first line.
func CompactContentEventLine(line string) string {
	if ev, ok := ParseContentEventLine(line); ok {
		return CompactContentEvent(ev)
	}
	return firstContentEventLine(line)
}

func firstContentEventLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
