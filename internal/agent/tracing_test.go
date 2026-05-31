package agent

import "testing"

func TestTraceLifecycle(t *testing.T) {
	trace := NewTrace("test-run")
	if trace.ID == "" {
		t.Error("trace should have an ID")
	}
	if trace.Name != "test-run" {
		t.Errorf("expected test-run, got %s", trace.Name)
	}

	span := NewSpan("model-call", "", GenerationSpanData{ResolvedModel: "gpt-4", PromptTokens: 100, UsageAvailable: true})
	trace.AddSpan(span)
	span.Finish()
	trace.Finish()

	if len(trace.Spans) != 1 {
		t.Errorf("expected 1 span, got %d", len(trace.Spans))
	}
	if span.DurationMS() < 0 {
		t.Error("duration should be non-negative")
	}
}

func TestMultiTracingProcessor(t *testing.T) {
	var calls1, calls2 int
	p1 := &countingProcessor{count: &calls1}
	p2 := &countingProcessor{count: &calls2}
	multi := &MultiTracingProcessor{Processors: []TracingProcessor{p1, p2}}

	trace := NewTrace("test")
	multi.OnTraceStart(trace)
	multi.OnTraceEnd(trace)
	multi.Flush()

	if calls1 != 3 || calls2 != 3 {
		t.Errorf("expected 3 calls each, got %d and %d", calls1, calls2)
	}
}

type countingProcessor struct {
	count *int
}

func (p *countingProcessor) OnTraceStart(*Trace) { *p.count++ }
func (p *countingProcessor) OnTraceEnd(*Trace)   { *p.count++ }
func (p *countingProcessor) OnSpanStart(*Span)   { *p.count++ }
func (p *countingProcessor) OnSpanEnd(*Span)     { *p.count++ }
func (p *countingProcessor) Flush()              { *p.count++ }
