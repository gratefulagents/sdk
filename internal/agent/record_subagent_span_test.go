package agent

import (
	"sync"
	"testing"
	"time"
)

// spanRecordingProcessor records span lifecycle events for assertions.
type spanRecordingProcessor struct {
	mu      sync.Mutex
	started []*Span
	ended   []*Span
}

func (p *spanRecordingProcessor) OnTraceStart(*Trace) {}
func (p *spanRecordingProcessor) OnTraceEnd(*Trace)   {}
func (p *spanRecordingProcessor) OnSpanStart(s *Span) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = append(p.started, s)
}
func (p *spanRecordingProcessor) OnSpanEnd(s *Span) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended = append(p.ended, s)
}
func (p *spanRecordingProcessor) Flush() {}

// RecordSubagentStarted + RecordSubagentCompleted must open and close ONE
// span whose time range covers the subagent's real lifetime and whose final
// data carries the completion metrics.
func TestSubagentSpanPairing(t *testing.T) {
	tp := &spanRecordingProcessor{}
	tracker := NewProgressTracker(WithTracingProcessor(tp))
	tracker.SetRootSpanID("root-1")

	tracker.RecordSubagentStarted("task-1", "tu-1", "explore the repo", "explore", "model-a", "shared", "prompt")
	time.Sleep(2 * time.Millisecond)
	tracker.RecordSubagentCompleted("task-1", "completed", "found it", 0.5, 3,
		Usage{InputTokens: 10, OutputTokens: 5}, "end_turn", []string{"a.go"}, nil)

	if len(tp.started) != 1 || len(tp.ended) != 1 {
		t.Fatalf("expected exactly one started and one ended span, got %d started, %d ended", len(tp.started), len(tp.ended))
	}
	if tp.started[0].ID != tp.ended[0].ID {
		t.Fatalf("start/end must be the same span: %s vs %s", tp.started[0].ID, tp.ended[0].ID)
	}
	span := tp.ended[0]
	if span.ParentID != "root-1" {
		t.Errorf("ParentID = %q, want root-1", span.ParentID)
	}
	if span.EndTime.IsZero() {
		t.Error("span must be finished")
	}
	data, ok := span.Data.(SubagentSpanData)
	if !ok {
		t.Fatalf("span data type = %T, want SubagentSpanData", span.Data)
	}
	if data.Status != "completed" {
		t.Errorf("Status = %q, want completed", data.Status)
	}
	if data.DurationMS <= 0 {
		t.Errorf("DurationMS = %d, want > 0 (real lifetime, not an instant span)", data.DurationMS)
	}
	if data.CostUSD != 0.5 || data.InputTokens != 10 || data.OutputTokens != 5 || data.TotalTokens != 15 {
		t.Errorf("final metrics not carried: %+v", data)
	}
	// Tracker must not leak the open-span entry.
	tracker.mu.Lock()
	leaked := len(tracker.subagentSpans)
	tracker.mu.Unlock()
	if leaked != 0 {
		t.Errorf("subagentSpans leaked %d entries", leaked)
	}
}

// A completion without a matching start (tracker restarted, processor wired
// late) must still emit a start/end pair so the completion data lands.
func TestSubagentCompletedWithoutStartFallsBack(t *testing.T) {
	tp := &spanRecordingProcessor{}
	tracker := NewProgressTracker(WithTracingProcessor(tp))

	tracker.RecordSubagentCompleted("task-x", "failed", "boom", 0, 1, Usage{}, "error", nil, nil)

	if len(tp.started) != 1 || len(tp.ended) != 1 {
		t.Fatalf("expected fallback start+end pair, got %d started, %d ended", len(tp.started), len(tp.ended))
	}
	if tp.started[0].ID != tp.ended[0].ID {
		t.Fatal("fallback start/end must be the same span")
	}
	data, ok := tp.ended[0].Data.(SubagentSpanData)
	if !ok {
		t.Fatalf("span data type = %T, want SubagentSpanData", tp.ended[0].Data)
	}
	if data.Status != "failed" {
		t.Errorf("Status = %q, want failed", data.Status)
	}
}
