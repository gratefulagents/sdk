package agent

import (
	"strings"
	"sync"
	"time"
)

const (
	// thinkingDeltaFlushInterval bounds how often coalesced reasoning chunks
	// are emitted. 500ms matches the platform's live-watch poll cadence, so a
	// finer granularity would be invisible while inflating the event log.
	thinkingDeltaFlushInterval = 500 * time.Millisecond
	// thinkingDeltaFlushBytes caps how much reasoning text is buffered before
	// an early flush, bounding memory and per-event payload size.
	thinkingDeltaFlushBytes = 4096
)

// thinkingDeltaEmitter coalesces streamed model reasoning text into periodic
// assistant_thinking_delta events so live consumers can render reasoning as
// it happens without one event per token. ToolUseID carries the llm attempt
// id, linking the chunk stream to its llm_attempt events and to the final
// assistant_thinking event that supersedes it (same id, complete text).
type thinkingDeltaEmitter struct {
	es        *EventStream
	attemptID string
	agentName string
	now       func() time.Time // test seam

	mu        sync.Mutex
	buf       strings.Builder
	lastFlush time.Time
}

func newThinkingDeltaEmitter(es *EventStream, attemptID, agentName string) *thinkingDeltaEmitter {
	return &thinkingDeltaEmitter{es: es, attemptID: attemptID, agentName: agentName, now: time.Now}
}

// Chunk buffers a reasoning text chunk, flushing when the flush interval has
// elapsed or the buffer is large. The first chunk flushes immediately (zero
// lastFlush) so the UI flips to live reasoning without waiting a full period.
// Race-free under concurrent use, but flush emission order is only guaranteed
// when callers serialize; transports deliver chunks sequentially.
func (e *thinkingDeltaEmitter) Chunk(text string) {
	if text == "" {
		return
	}
	e.mu.Lock()
	e.buf.WriteString(text)
	var flushText string
	if e.now().Sub(e.lastFlush) >= thinkingDeltaFlushInterval || e.buf.Len() >= thinkingDeltaFlushBytes {
		flushText = e.buf.String()
		e.buf.Reset()
		e.lastFlush = e.now()
	}
	e.mu.Unlock()
	if flushText != "" {
		e.es.EmitThinkingDelta(flushText, e.agentName, e.attemptID)
	}
}

// Flush emits any buffered residue. Called once after the model call returns
// so the tail of the reasoning stream is not lost to the flush interval.
func (e *thinkingDeltaEmitter) Flush() {
	e.mu.Lock()
	flushText := e.buf.String()
	e.buf.Reset()
	e.lastFlush = e.now()
	e.mu.Unlock()
	if flushText != "" {
		e.es.EmitThinkingDelta(flushText, e.agentName, e.attemptID)
	}
}
