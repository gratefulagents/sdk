package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/internal/modeldelta"
)

func decodeContentEvents(t *testing.T, buf *bytes.Buffer) []ContentEvent {
	t.Helper()
	var events []ContentEvent
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var ev ContentEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal event line %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func eventsOfType(events []ContentEvent, typ string) []ContentEvent {
	var out []ContentEvent
	for _, ev := range events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func TestThinkingDeltaEmitterCoalesces(t *testing.T) {
	var buf bytes.Buffer
	es := NewEventStream(&buf)
	now := time.Unix(1000, 0)
	e := newThinkingDeltaEmitter(es, "attempt-1", "worker")
	e.now = func() time.Time { return now }

	// First chunk flushes immediately (zero lastFlush) so the UI flips to
	// live reasoning without waiting a full interval.
	e.Chunk("first ")
	// Within the flush interval: buffered, not emitted.
	now = now.Add(100 * time.Millisecond)
	e.Chunk("second ")
	now = now.Add(100 * time.Millisecond)
	e.Chunk("third ")
	// Interval elapsed: buffered chunks flush together.
	now = now.Add(thinkingDeltaFlushInterval)
	e.Chunk("fourth ")
	// Residue after the model call returns.
	e.Chunk("tail")
	e.Flush()
	// Empty flush emits nothing.
	e.Flush()
	// Empty chunks are ignored.
	e.Chunk("")

	deltas := eventsOfType(decodeContentEvents(t, &buf), "assistant_thinking_delta")
	if len(deltas) != 3 {
		t.Fatalf("delta events = %d, want 3", len(deltas))
	}
	wantMessages := []string{"first ", "second third fourth ", "tail"}
	for i, want := range wantMessages {
		if deltas[i].Message != want {
			t.Errorf("delta[%d].Message = %q, want %q", i, deltas[i].Message, want)
		}
		if deltas[i].ToolUseID != "attempt-1" {
			t.Errorf("delta[%d].ToolUseID = %q, want attempt-1", i, deltas[i].ToolUseID)
		}
		if deltas[i].AgentName != "worker" {
			t.Errorf("delta[%d].AgentName = %q, want worker", i, deltas[i].AgentName)
		}
	}
}

func TestThinkingDeltaEmitterFlushesOnSize(t *testing.T) {
	var buf bytes.Buffer
	es := NewEventStream(&buf)
	now := time.Unix(1000, 0)
	e := newThinkingDeltaEmitter(es, "attempt-1", "worker")
	e.now = func() time.Time { return now }

	e.Chunk("warmup") // immediate first flush, resets the timer
	big := strings.Repeat("x", thinkingDeltaFlushBytes)
	e.Chunk(big) // exceeds the size cap inside the interval

	deltas := eventsOfType(decodeContentEvents(t, &buf), "assistant_thinking_delta")
	if len(deltas) != 2 {
		t.Fatalf("delta events = %d, want 2", len(deltas))
	}
	if deltas[1].Message != big {
		t.Errorf("size-triggered flush lost text: got %d bytes, want %d", len(deltas[1].Message), len(big))
	}
}

// reasoningSinkModel simulates a transport that streams reasoning internally
// during a blocking GetResponse, feeding any installed context sink.
type reasoningSinkModel struct {
	*mockModel
	chunks []string
}

func (m *reasoningSinkModel) GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	if sink := modeldelta.ReasoningSinkFromContext(ctx); sink != nil {
		for _, c := range m.chunks {
			sink(c)
		}
	}
	return m.mockModel.GetResponse(ctx, req)
}

func (m *reasoningSinkModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	resp, err := m.mockModel.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan ModelStreamEvent, len(m.chunks)+1)
	done := make(chan *ModelResponse, 1)
	for _, chunk := range m.chunks {
		events <- ModelStreamEvent{Type: ModelStreamReasoningDelta, Delta: chunk}
	}
	events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
	close(events)
	done <- resp
	return NewModelStream(events, done), nil
}

func TestRunnerStreamsThinkingDeltasToEventStream(t *testing.T) {
	model := &reasoningSinkModel{
		mockModel: &mockModel{
			responses: []*ModelResponse{{
				Items: []RunItem{
					{Type: RunItemReasoning, Reasoning: &ReasoningData{Text: "let me think about this"}},
					{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}},
				},
			}},
		},
		chunks: []string{"let me think", " about this"},
	}
	runner := NewRunnerWithModel(model)
	var buf bytes.Buffer
	hooks := NewPlatformHooks(NewProgressTracker(), NewEventStream(&buf))

	result, err := runner.Run(context.Background(), &Agent{Name: "worker"}, nil, RunConfig{
		Hooks:    hooks,
		MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "done" {
		t.Fatalf("FinalText() = %q, want done", result.FinalText())
	}

	events := decodeContentEvents(t, &buf)
	attempts := eventsOfType(events, "llm_attempt")
	if len(attempts) == 0 || attempts[0].ToolUseID == "" {
		t.Fatalf("expected llm_attempt events with a ToolUseID, got %+v", attempts)
	}
	attemptID := attempts[0].ToolUseID

	deltas := eventsOfType(events, "assistant_thinking_delta")
	if len(deltas) == 0 {
		t.Fatal("expected assistant_thinking_delta events, got none")
	}
	var streamed strings.Builder
	for _, d := range deltas {
		streamed.WriteString(d.Message)
		if d.ToolUseID != attemptID {
			t.Errorf("delta ToolUseID = %q, want llm attempt id %q", d.ToolUseID, attemptID)
		}
		if d.AgentName != "worker" {
			t.Errorf("delta AgentName = %q, want worker", d.AgentName)
		}
	}
	if streamed.String() != "let me think about this" {
		t.Errorf("streamed text = %q, want full reasoning", streamed.String())
	}

	finals := eventsOfType(events, "assistant_thinking")
	if len(finals) != 1 {
		t.Fatalf("assistant_thinking events = %d, want 1", len(finals))
	}
	if finals[0].Message != "let me think about this" {
		t.Errorf("final Message = %q", finals[0].Message)
	}
	if finals[0].ToolUseID != attemptID {
		t.Errorf("final ToolUseID = %q, want llm attempt id %q so consumers can supersede the delta stream", finals[0].ToolUseID, attemptID)
	}
}

func TestRunStreamedDoesNotEmitThinkingDeltaEvents(t *testing.T) {
	// The streamed-run path surfaces reasoning through its own event channel;
	// the JSONL event stream must not receive duplicate delta events there.
	model := &reasoningSinkModel{
		mockModel: &mockModel{
			responses: []*ModelResponse{{
				Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}},
			}},
		},
		chunks: []string{"hidden reasoning"},
	}
	runner := NewRunnerWithModel(model)
	var buf bytes.Buffer
	hooks := NewPlatformHooks(NewProgressTracker(), NewEventStream(&buf))

	streamed := runner.RunStreamed(context.Background(), &Agent{Name: "worker"}, nil, RunConfig{
		Hooks:    hooks,
		MaxTurns: 2,
	})
	for range streamed.Events {
	}
	if err := streamed.Err(); err != nil {
		t.Fatal(err)
	}
	if deltas := eventsOfType(decodeContentEvents(t, &buf), "assistant_thinking_delta"); len(deltas) != 0 {
		t.Fatalf("streamed run emitted %d assistant_thinking_delta events, want 0", len(deltas))
	}
}
