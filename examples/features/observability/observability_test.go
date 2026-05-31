package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestObservabilityExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	tracing := &recordingTracingProcessor{}
	tracker := agentsdk.NewProgressTracker(agentsdk.WithTracingProcessor(tracing))
	var eventBytes bytes.Buffer
	eventStream := agentsdk.NewEventStream(&eventBytes)
	hooks := agentsdk.NewPlatformHooks(tracker, eventStream)

	echoTool := &agentsdk.FunctionTool{
		ToolName:        "echo",
		ToolDescription: "Echo text. Always call this when asked to echo.",
		Schema:          json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		ReadOnly:        true,
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "echoed", nil
		},
	}

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "observable",
		Model:        model,
		Instructions: "When asked to echo, call the echo tool, then briefly summarize.",
		Tools:        []agentsdk.Tool{echoTool},
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Echo the word hello using the echo tool."},
		},
	}, agentsdk.RunConfig{
		MaxTurns: 4,

		Hooks:            hooks,
		TracingProcessor: tracing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() == "" {
		t.Fatalf("FinalText() empty")
	}
	if snapshot := tracker.Snapshot(); snapshot.ToolCallCount == 0 {
		t.Fatalf("tool calls = %d, want >=1", snapshot.ToolCallCount)
	}
	events := eventBytes.String()
	if !strings.Contains(events, `"type":"tool_start"`) || !strings.Contains(events, `"type":"assistant_text"`) {
		t.Fatalf("expected tool and text events, got:\n%s", events)
	}
	if !tracing.sawEndedSpan("generation") || !tracing.sawEndedSpan("function") {
		t.Fatalf("ended spans = %#v", tracing.EndedSpans())
	}
}

type recordingTracingProcessor struct {
	mu    sync.Mutex
	ended []string
}

func (p *recordingTracingProcessor) OnTraceStart(*agentsdk.Trace) {}
func (p *recordingTracingProcessor) OnTraceEnd(*agentsdk.Trace)   {}
func (p *recordingTracingProcessor) OnSpanStart(*agentsdk.Span)   {}

func (p *recordingTracingProcessor) OnSpanEnd(span *agentsdk.Span) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended = append(p.ended, span.Name)
}

func (p *recordingTracingProcessor) Flush() {}

func (p *recordingTracingProcessor) sawEndedSpan(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ended := range p.ended {
		if ended == name {
			return true
		}
	}
	return false
}

func (p *recordingTracingProcessor) EndedSpans() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.ended))
	copy(out, p.ended)
	return out
}
