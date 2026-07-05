package otel

import (
	"context"
	"testing"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// newTestProcessor builds a processor backed by an in-memory exporter with a
// synchronous span processor so ended spans are observable immediately.
func newTestProcessor() (*OTelTracingProcessor, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return &OTelTracingProcessor{
		tracer:   provider.Tracer("test"),
		provider: provider,
		spans:    make(map[string]oteltrace.Span),
		ctxs:     make(map[string]context.Context),
	}, exp
}

func (o *OTelTracingProcessor) liveTraceIDFor(spanID string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	sp, ok := o.spans[spanID]
	if !ok {
		return ""
	}
	return sp.SpanContext().TraceID().String()
}

// Spans whose parent already ended (async subagents parented under their
// spawn tool call) must stay in the run's trace, not mint a new one.
func TestSpanJoinsTraceAfterParentEnded(t *testing.T) {
	p, _ := newTestProcessor()

	tr := agent.NewTrace("run")
	p.OnTraceStart(tr)
	want := p.TraceID()
	if want == "" {
		t.Fatal("expected root trace ID")
	}

	spawn := agent.NewSpan("spawn", tr.ID, &agent.FunctionSpanData{ToolName: "subagent"})
	p.OnSpanStart(spawn)
	spawn.Finish()
	p.OnSpanEnd(spawn)

	// Child starts AFTER its parent span ended: parent ctx must be retained.
	child := agent.NewSpan("child", spawn.ID, &agent.FunctionSpanData{ToolName: "grep"})
	p.OnSpanStart(child)
	if got := p.liveTraceIDFor(child.ID); got != want {
		t.Errorf("child after parent end: trace ID = %s, want %s", got, want)
	}
	child.Finish()
	p.OnSpanEnd(child)

	// Completely unknown parent: falls back to the trace root.
	orphan := agent.NewSpan("orphan", "no-such-span", &agent.FunctionSpanData{ToolName: "x"})
	p.OnSpanStart(orphan)
	if got := p.liveTraceIDFor(orphan.ID); got != want {
		t.Errorf("orphan span: trace ID = %s, want %s", got, want)
	}
	orphan.Finish()
	p.OnSpanEnd(orphan)

	// Straggler after the trace ended still joins the last trace.
	tr.Finish()
	p.OnTraceEnd(tr)
	late := agent.NewSpan("late", tr.ID, &agent.FunctionSpanData{ToolName: "y"})
	p.OnSpanStart(late)
	if got := p.liveTraceIDFor(late.ID); got != want {
		t.Errorf("late span after trace end: trace ID = %s, want %s", got, want)
	}
	late.Finish()
	p.OnSpanEnd(late)
}

func attrValue(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func findSpan(t *testing.T, exp *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("span %q not exported; have %d spans", name, len(exp.GetSpans()))
	return tracetest.SpanStub{}
}

// Span data is mutated between start and end (tool output, final status,
// usage): the exported span must carry the FINAL values and an error status
// for failed tools/generations/subagents.
func TestOnSpanEndExportsFinalDataAndErrorStatus(t *testing.T) {
	p, exp := newTestProcessor()
	tr := agent.NewTrace("run")
	p.OnTraceStart(tr)

	// Failed tool: output only exists at end.
	fn := agent.NewSpan("function", tr.ID, &agent.FunctionSpanData{ToolName: "bash", Input: "rm -x"})
	p.OnSpanStart(fn)
	fn.Data = &agent.FunctionSpanData{ToolName: "bash", Input: "rm -x", Output: "boom", IsError: true}
	fn.Finish()
	p.OnSpanEnd(fn)

	got := findSpan(t, exp, "tool.bash")
	if v, ok := attrValue(got, "tool.output"); !ok || v.AsString() != "boom" {
		t.Errorf("tool.output = %v, want boom (final data must be exported)", v.Emit())
	}
	if v, ok := attrValue(got, "error"); !ok || !v.AsBool() {
		t.Error("expected error=true attribute on failed tool span")
	}
	if got.Status.Code != codes.Error {
		t.Errorf("tool span status = %v, want Error", got.Status.Code)
	}

	// Failed generation: gen.success=false + error status.
	gen := agent.NewSpan("generation", tr.ID, &agent.GenerationSpanData{RequestedModel: "m"})
	p.OnSpanStart(gen)
	gen.Data = &agent.GenerationSpanData{RequestedModel: "m", Success: false, Status: "failed", Error: "rate limited"}
	gen.Finish()
	p.OnSpanEnd(gen)

	genStub := findSpan(t, exp, "llm.generation")
	if genStub.Status.Code != codes.Error {
		t.Errorf("failed generation status = %v, want Error", genStub.Status.Code)
	}
	if v, ok := attrValue(genStub, "gen.error"); !ok || v.AsString() != "rate limited" {
		t.Errorf("gen.error attr = %v, want 'rate limited'", v.Emit())
	}

	// Successful generation: no error status.
	ok2 := agent.NewSpan("generation", tr.ID, &agent.GenerationSpanData{RequestedModel: "m2"})
	p.OnSpanStart(ok2)
	ok2.Data = &agent.GenerationSpanData{RequestedModel: "m2", Success: true, Status: "completed"}
	ok2.Finish()
	p.OnSpanEnd(ok2)
	var okStub tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == "llm.generation" {
			okStub = s // last one wins; both named llm.generation
		}
	}
	if okStub.Status.Code == codes.Error {
		t.Error("successful generation must not carry an error status")
	}

	// Failed subagent: status=failed → error status + final metrics attrs.
	sub := agent.NewSpan("subagent.explore", tr.ID, agent.SubagentSpanData{TaskID: "t1", Type: "explore", Status: "initializing"})
	p.OnSpanStart(sub)
	sub.Data = agent.SubagentSpanData{TaskID: "t1", Type: "explore", Status: "failed", ResultText: "exploded", CostUSD: 0.25, DurationMS: 1234}
	sub.Finish()
	p.OnSpanEnd(sub)

	subStub := findSpan(t, exp, "subagent.explore")
	if subStub.Status.Code != codes.Error {
		t.Errorf("failed subagent status = %v, want Error", subStub.Status.Code)
	}
	if v, ok := attrValue(subStub, "subagent.status"); !ok || v.AsString() != "failed" {
		t.Errorf("subagent.status attr = %v, want failed (final data must be exported)", v.Emit())
	}
	if v, ok := attrValue(subStub, "subagent.duration_ms"); !ok || v.AsInt64() != 1234 {
		t.Errorf("subagent.duration_ms attr = %v, want 1234", v.Emit())
	}

	tr.Finish()
	p.OnTraceEnd(tr)
}
