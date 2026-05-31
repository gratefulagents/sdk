package events

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// Kind identifies an SDK runtime event emitted to a host adapter.
type Kind string

const (
	KindStatus    Kind = "status"
	KindDelta     Kind = "delta"
	KindItem      Kind = "item"
	KindTrace     Kind = "trace"
	KindLog       Kind = "log"
	KindDone      Kind = "done"
	KindError     Kind = "error"
	KindChildTool Kind = "child_tool"
	KindSubAgent  Kind = "subagent"
	KindPhase     Kind = "phase"
)

// Event is the typed SDK event shape hosts can render or persist.
type Event struct {
	Kind     Kind
	Text     string
	Item     *agentsdk.RunItem
	Result   *agentsdk.RunResult
	Err      error
	History  []agentsdk.RunItem
	Pending  *agentsdk.Interruption
	Snapshot agentsdk.ProgressSnapshot
	Child    *agentsdk.ChildToolEvent
	SubAgent *agentsdk.SubAgentStreamEvent
}

// Sink consumes typed runtime events.
type Sink interface {
	Emit(Event)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(Event)

func (f SinkFunc) Emit(ev Event) {
	if f != nil {
		f(ev)
	}
}

// LineWriter turns JSONL content events into typed SDK events.
//
// Hosts can attach this to SessionEventStream and render typed events instead
// of reparsing event JSON themselves.
type LineWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	sink   Sink
}

func NewLineWriter(sink Sink) *LineWriter {
	return &LineWriter{sink: sink}
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, _ := w.buffer.Write(p)
	scanner := bufio.NewScanner(bytes.NewReader(w.buffer.Bytes()))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	// If a single line exceeds the scanner's max token size, Scan stops early
	// with an error and leaves the oversized content unconsumed. Surface it as
	// one (truncated) line rather than silently dropping output.
	if err := scanner.Err(); err != nil {
		lines = append(lines, w.buffer.String())
		w.buffer.Reset()
		for _, line := range lines {
			if strings.TrimSpace(line) == "" || w.sink == nil {
				continue
			}
			w.sink.Emit(Event{Kind: KindLog, Text: agentsdk.CompactContentEventLine(line)})
		}
		return n, nil
	}
	if len(p) > 0 && p[len(p)-1] != '\n' {
		w.buffer.Reset()
		if len(lines) > 0 {
			last := lines[len(lines)-1]
			lines = lines[:len(lines)-1]
			w.buffer.WriteString(last)
		}
	} else {
		w.buffer.Reset()
	}

	for _, line := range lines {
		if strings.TrimSpace(line) == "" || w.sink == nil {
			continue
		}
		if ev, ok := agentsdk.ParseContentEventLine(line); ok {
			if subagent, ok := agentsdk.SubAgentStreamEventFromContentEvent(ev); ok {
				w.sink.Emit(Event{Kind: KindSubAgent, SubAgent: subagent})
				continue
			}
			if child, ok := agentsdk.ChildToolEventFromContentEvent(ev); ok {
				w.sink.Emit(Event{Kind: KindChildTool, Child: child})
				continue
			}
		}
		w.sink.Emit(Event{Kind: KindLog, Text: agentsdk.CompactContentEventLine(line)})
	}
	return n, nil
}

var _ io.Writer = (*LineWriter)(nil)
