package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func orderRecordingTool(name string, readOnly bool, delay time.Duration, order *[]string, mu *sync.Mutex) *FunctionTool {
	return &FunctionTool{
		ToolName: name,
		Schema:   json.RawMessage(`{"type":"object"}`),
		ReadOnly: readOnly,
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			time.Sleep(delay)
			mu.Lock()
			*order = append(*order, string(input))
			mu.Unlock()
			return "ok", nil
		},
	}
}

func TestExecuteToolsRunsMutatingCallsInCallOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	slow := orderRecordingTool("slow_write", false, 40*time.Millisecond, &order, &mu)
	fast := orderRecordingTool("fast_write", false, 0, &order, &mu)
	read := orderRecordingTool("read", true, 0, &order, &mu)
	tools := []Tool{slow, fast, read}
	calls := []ToolCallData{
		{ID: "1", Name: "slow_write", Input: json.RawMessage(`"w1"`)},
		{ID: "2", Name: "fast_write", Input: json.RawMessage(`"w2"`)},
		{ID: "3", Name: "read", Input: json.RawMessage(`"r3"`)},
		{ID: "4", Name: "fast_write", Input: json.RawMessage(`"w4"`)},
	}
	cfg := RunConfig{}
	runCtx := newRunContext(context.Background(), cfg)
	agent := &Agent{Name: "a", Tools: tools}
	for i := 0; i < 5; i++ {
		order = nil
		results, _, _, _, _, err := NewRunnerWithModel(&mockModel{}).executeTools(context.Background(), runCtx, agent, tools, calls, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(order, ","); got != `"w1","w2","r3","w4"` {
			t.Fatalf("execution order = %s, want call order", got)
		}
		for j, item := range results {
			if item.ToolOutput == nil || item.ToolOutput.CallID != calls[j].ID {
				t.Fatalf("result slot %d = %+v, want call %s", j, item, calls[j].ID)
			}
		}
	}
}

func TestExecuteToolsReadOnlyCallsRunConcurrently(t *testing.T) {
	var inFlight, peak atomic.Int32
	read := &FunctionTool{
		ToolName: "read",
		Schema:   json.RawMessage(`{"type":"object"}`),
		ReadOnly: true,
		Fn: func(context.Context, json.RawMessage) (string, error) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			inFlight.Add(-1)
			return "ok", nil
		},
	}
	calls := []ToolCallData{{ID: "1", Name: "read"}, {ID: "2", Name: "read"}, {ID: "3", Name: "read"}}
	cfg := RunConfig{}
	_, _, _, _, _, err := NewRunnerWithModel(&mockModel{}).executeTools(context.Background(), newRunContext(context.Background(), cfg), &Agent{Name: "a"}, []Tool{read}, calls, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrent read-only calls = %d, want concurrency", peak.Load())
	}
}

func TestExecuteToolsSubAgentDoesNotBlockLaterMutation(t *testing.T) {
	released := make(chan struct{})
	delegate := &FunctionTool{
		ToolName: "subagent",
		Schema:   json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			select {
			case <-released:
				return "joined", nil
			case <-time.After(5 * time.Second):
				return "", errors.New("mutation was serialized behind the sub-agent")
			}
		},
	}
	write := &FunctionTool{
		ToolName: "write",
		Schema:   json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			close(released)
			return "written", nil
		},
	}
	calls := []ToolCallData{{ID: "1", Name: "subagent"}, {ID: "2", Name: "write"}}
	cfg := RunConfig{}
	results, _, _, _, _, err := NewRunnerWithModel(&mockModel{}).executeTools(context.Background(), newRunContext(context.Background(), cfg), &Agent{Name: "a"}, []Tool{delegate, write}, calls, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].ToolOutput.IsError || results[0].ToolOutput.Content != "joined" {
		t.Fatalf("sub-agent result = %+v", results[0].ToolOutput)
	}
}

func TestExecuteToolsRecoversMutatingToolPanic(t *testing.T) {
	boom := &FunctionTool{ToolName: "boom", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) { panic("kaboom") }}
	cfg := RunConfig{}
	results, _, _, _, _, err := NewRunnerWithModel(&mockModel{}).executeTools(context.Background(), newRunContext(context.Background(), cfg), &Agent{Name: "a"}, []Tool{boom}, []ToolCallData{{ID: "1", Name: "boom"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].ToolOutput.IsError || !strings.Contains(results[0].ToolOutput.Content, "panicked") {
		t.Fatalf("panic result = %+v", results[0].ToolOutput)
	}
}

type spanRecorder struct {
	mu    sync.Mutex
	ended []*Span
}

func (*spanRecorder) OnTraceStart(*Trace) {}
func (*spanRecorder) OnTraceEnd(*Trace)   {}
func (*spanRecorder) OnSpanStart(*Span)   {}
func (r *spanRecorder) OnSpanEnd(s *Span) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, s)
}
func (*spanRecorder) Flush() {}

func (r *spanRecorder) generations() []*GenerationSpanData {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*GenerationSpanData
	for _, s := range r.ended {
		if d, ok := s.Data.(*GenerationSpanData); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestRetryPolicyRetryDoesNotConsumeTurn(t *testing.T) {
	model := &mockModel{
		errors:    []error{errors.New("503 service unavailable"), errors.New("503 service unavailable"), nil},
		responses: []*ModelResponse{nil, nil, finalResponse("recovered")},
	}
	rec := &spanRecorder{}
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a"}, nil, RunConfig{
		MaxTurns:         1,
		TracingProcessor: rec,
		RetryPolicy:      &RetryPolicy{MaxRetries: 2, Backoff: RetryBackoffSettings{InitialDelayMS: 1, MaxDelayMS: 1, Multiplier: 1}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want retries within the single turn", err)
	}
	if result.FinalText() != "recovered" {
		t.Fatalf("final = %q", result.FinalText())
	}
	gens := rec.generations()
	if len(gens) != 3 {
		t.Fatalf("generation attempts = %d, want 3", len(gens))
	}
	for _, g := range gens {
		if g.Turn != 1 {
			t.Fatalf("attempt %d turn = %d, want all attempts on turn 1", g.AttemptNumber, g.Turn)
		}
	}
}

func TestProviderAdviceRetryDoesNotConsumeTurn(t *testing.T) {
	model := &mockModel{
		errors:       []error{errors.New("429 rate limited"), nil},
		responses:    []*ModelResponse{nil, finalResponse("ok")},
		retryAdvices: []ModelRetryAdvice{{ShouldRetry: true, RetryAfterMS: 1}},
	}
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a"}, nil, RunConfig{MaxTurns: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FinalText() != "ok" {
		t.Fatalf("final = %q", result.FinalText())
	}
}

func TestForcedCompactionRecoveryIsBoundedPerTurn(t *testing.T) {
	overflow := errors.New("API request failed with status 400: prompt is too long: 210000 tokens > 200000 maximum")
	model := &mockModel{errors: []error{overflow, overflow, overflow, overflow}}
	input := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "task"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "more"}},
	}
	for i := 0; i < 20; i++ {
		input = append(input, RunItem{Type: RunItemMessage, Agent: &Agent{Name: "a"}, Message: &MessageOutput{Text: strings.Repeat("history ", 200)}})
	}
	compactions := 0
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a"}, input, RunConfig{
		MaxTurns:         5,
		CompactionConfig: CompactionConfig{Enabled: true},
		CompactionRecorder: func(before, after int, _ string) {
			compactions++
			if after >= before {
				t.Errorf("compaction before=%d after=%d, want a reduction on a consistent basis", before, after)
			}
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want overflow failure")
	}
	var maxTurns *MaxTurnsExceeded
	if errors.As(err, &maxTurns) {
		t.Fatalf("Run() error = %v, recovery must not consume turns", err)
	}
	if compactions != 1 {
		t.Fatalf("forced compactions = %d, want exactly one for the Anthropic overflow", compactions)
	}
	if model.callIdx != 2 {
		t.Fatalf("model calls = %d, want one forced-compaction retry per turn", model.callIdx)
	}
}

func TestIsContextLengthExceededErrorPhrasings(t *testing.T) {
	for _, msg := range []string{
		"context_length_exceeded",
		"Your input exceeds the context window of this model",
		`API request failed with status 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`,
		"This model's maximum context length is 128000 tokens",
		"Input is too long for requested model.",
		"Request has too many tokens for the model",
		"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576)",
	} {
		if !isContextLengthExceededError(errors.New(msg)) {
			t.Errorf("isContextLengthExceededError(%q) = false, want true", msg)
		}
	}
	for _, msg := range []string{
		"Too many tokens, please wait before trying again.",
		"429 rate limit: too many tokens per minute",
		"internal server error",
	} {
		if isContextLengthExceededError(errors.New(msg)) {
			t.Errorf("isContextLengthExceededError(%q) = true, want false", msg)
		}
	}
}

type manyDeltaModel struct{ deltas int }

func (*manyDeltaModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, errors.New("unused")
}

func (m *manyDeltaModel) StreamResponse(context.Context, ModelRequest) (*ModelStream, error) {
	events := make(chan ModelStreamEvent, m.deltas+1)
	done := make(chan *ModelResponse, 1)
	var text strings.Builder
	for i := 0; i < m.deltas; i++ {
		events <- ModelStreamEvent{Type: ModelStreamDelta, Delta: "x"}
		text.WriteString("x")
	}
	resp := &ModelResponse{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: text.String()}}}}
	events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
	close(events)
	done <- resp
	return NewModelStream(events, done), nil
}

func (*manyDeltaModel) GetRetryAdvice(error) *ModelRetryAdvice { return nil }
func (*manyDeltaModel) CalculateCost(Usage) float64            { return 0 }
func (*manyDeltaModel) Provider() string                       { return "deltas" }

func TestRunStreamedFinalResultWithoutDrainingEvents(t *testing.T) {
	streamed := NewRunnerWithModel(&manyDeltaModel{deltas: 500}).RunStreamed(context.Background(), &Agent{Name: "a"}, nil, RunConfig{MaxTurns: 1})
	got := make(chan *RunResult, 1)
	go func() { got <- streamed.FinalResult() }()
	select {
	case result := <-got:
		if err := streamed.Err(); err != nil {
			t.Fatalf("Err() = %v", err)
		}
		if len(result.FinalText()) != 500 {
			t.Fatalf("final text len = %d, want 500", len(result.FinalText()))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FinalResult hung while Events was never drained")
	}
}

func TestRunStreamedSlowConsumerIsNotProviderIdleness(t *testing.T) {
	streamed := NewRunnerWithModel(&manyDeltaModel{deltas: 200}).RunStreamed(context.Background(), &Agent{Name: "a"}, nil, RunConfig{
		MaxTurns:         1,
		ModelCallTimeout: 30 * time.Millisecond,
	})
	time.Sleep(150 * time.Millisecond) // buffer fills; run blocks on the consumer
	deltas := 0
	for ev := range streamed.Events {
		if ev.Type == StreamEventRawResponse {
			deltas++
		}
	}
	if err := streamed.Err(); err != nil {
		t.Fatalf("Err() = %v, slow consumer must not trip the provider idle timeout", err)
	}
	if deltas != 200 {
		t.Fatalf("deltas = %d, want all 200 delivered to an attached consumer", deltas)
	}
}

// pendingJoinTool always reports pending sub-agent results, modeling a model
// that keeps spawning sub-agents.
type pendingJoinTool struct {
	*FunctionTool
	joins atomic.Int32
}

func (*pendingJoinTool) HasPendingSubAgentFinalJoin() bool { return true }
func (p *pendingJoinTool) JoinSubAgentResults(context.Context) ([]RunItem, error) {
	p.joins.Add(1)
	return []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "[sub-agent result]"}}}, nil
}

func TestSubAgentFinalJoinExtensionsAreCapped(t *testing.T) {
	var responses []*ModelResponse
	for i := 0; i < 20; i++ {
		responses = append(responses, finalResponse("done"))
	}
	model := &mockModel{responses: responses}
	join := &pendingJoinTool{FunctionTool: &FunctionTool{ToolName: "subagent", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }}}
	_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a", Tools: []Tool{join}}, nil, RunConfig{MaxTurns: 1})
	var maxTurns *MaxTurnsExceeded
	if !errors.As(err, &maxTurns) {
		t.Fatalf("Run() error = %v, want MaxTurnsExceeded once extensions are exhausted", err)
	}
	if want := 1 + maxSubAgentFinalJoinExtensions; model.callIdx != want {
		t.Fatalf("model calls = %d, want %d", model.callIdx, want)
	}
	for i, req := range model.requests[1:] {
		if len(req.Tools) != 0 {
			t.Fatalf("extension request %d has %d tools, want none", i+1, len(req.Tools))
		}
		if !strings.Contains(req.Instructions, "<final_turn>") {
			t.Fatalf("extension request %d lacks the final summary directive", i+1)
		}
	}
}

type panickingAuthorizer struct{}

func (panickingAuthorizer) Authorize(context.Context, ActionRequest) (ActionAuthorization, error) {
	panic("authorizer bug")
}

func TestUserCallbackPanicsBecomeErrors(t *testing.T) {
	t.Run("final answer verifier", func(t *testing.T) {
		model := &mockModel{responses: []*ModelResponse{finalResponse("answer")}}
		tool := &FunctionTool{ToolName: "t", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }}
		result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a", Tools: []Tool{tool}}, nil, RunConfig{
			FinalAnswerVerifier: func(context.Context, string) (string, error) { panic("verifier bug") },
		})
		if err != nil || result.FinalText() != "answer" {
			t.Fatalf("Run() = %v, %v; verifier panic should be logged and ignored", result, err)
		}
	})
	t.Run("immediate input poller", func(t *testing.T) {
		model := &mockModel{responses: []*ModelResponse{finalResponse("answer")}}
		_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a"}, nil, RunConfig{
			ImmediateInputPoller: func(context.Context) ([]RunItem, error) { panic("poller bug") },
		})
		if err != nil {
			t.Fatalf("Run() error = %v; poller panic should be logged like a poll error", err)
		}
	})
	t.Run("immediate input finalizer", func(t *testing.T) {
		model := &mockModel{responses: []*ModelResponse{finalResponse("answer")}}
		_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a"}, nil, RunConfig{
			ImmediateInputFinalizer: func(context.Context) ([]RunItem, error) { panic("finalizer bug") },
		})
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("Run() error = %v, want panic converted to error", err)
		}
	})
	t.Run("handoff callbacks", func(t *testing.T) {
		for name, opt := range map[string]HandoffOption{
			"on handoff":   WithOnHandoff(func(*RunContext, json.RawMessage) { panic("on handoff bug") }),
			"input filter": WithInputFilter(func([]RunItem, []RunItem) []RunItem { panic("filter bug") }),
		} {
			target := &Agent{Name: "target"}
			h := NewHandoff(target, opt)
			model := &mockModel{responses: []*ModelResponse{
				{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "h1", Name: h.ToolName, Input: json.RawMessage(`{}`)}}}},
			}}
			_, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a", Handoffs: []*Handoff{h}}, nil, RunConfig{})
			if err == nil || !strings.Contains(err.Error(), "panicked") {
				t.Fatalf("%s: Run() error = %v, want panic converted to error", name, err)
			}
		}
	})
	t.Run("action authorizer", func(t *testing.T) {
		tool := &FunctionTool{ToolName: "t", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) { return "ran", nil }}
		cfg := RunConfig{ActionAuthorizer: panickingAuthorizer{}}
		results, _, _, audits, _, err := NewRunnerWithModel(&mockModel{}).executeTools(context.Background(), newRunContext(context.Background(), cfg), &Agent{Name: "a"}, []Tool{tool}, []ToolCallData{{ID: "1", Name: "t"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !results[0].ToolOutput.IsError || len(audits) != 1 || !strings.Contains(audits[0].Error, "panicked") {
			t.Fatalf("results = %+v audits = %+v, want fail-closed denial", results[0].ToolOutput, audits)
		}
	})
}

type failingPostToolHook struct {
	NoOpRunHooks
	failTool string
}

func (h failingPostToolHook) OnToolEndError(_ *RunContext, _ *Agent, _ Tool, call ToolCallData, _ ToolResult) error {
	if call.Name == h.failTool {
		return errors.New("audit sink down")
	}
	return nil
}

type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, ActionRequest) (ActionAuthorization, error) {
	return ActionAuthorization{Decision: ActionDecisionAllow}, nil
}

func TestFatalToolBatchErrorKeepsCompletedCalls(t *testing.T) {
	okTool := func(name string) *FunctionTool {
		return &FunctionTool{ToolName: name, Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) { return name + " done", nil }}
	}
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "fails", Input: json.RawMessage(`{}`)}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "push", Input: json.RawMessage(`{}`)}},
	}}}}
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a", Tools: []Tool{okTool("fails"), okTool("push")}}, nil, RunConfig{
		Hooks:            failingPostToolHook{failTool: "fails"},
		ActionAuthorizer: allowAuthorizer{},
	})
	if err == nil || !strings.Contains(err.Error(), "post-tool hook failed") {
		t.Fatalf("Run() error = %v, want post-tool hook failure", err)
	}
	if result == nil {
		t.Fatal("partial result = nil")
	}
	if len(result.ActionAuditRecords) != 2 {
		t.Fatalf("audits = %d, want both executed calls", len(result.ActionAuditRecords))
	}
	outputs := map[string]bool{}
	for _, item := range result.NewItems {
		if item.ToolOutput != nil {
			outputs[item.ToolOutput.CallID] = true
		}
	}
	if !outputs["c1"] || !outputs["c2"] {
		t.Fatalf("partial NewItems outputs = %v, want both calls", outputs)
	}
	historyOutputs := 0
	for _, item := range result.FinalHistory {
		if item.ToolOutput != nil {
			historyOutputs++
		}
	}
	if historyOutputs != 2 {
		t.Fatalf("FinalHistory tool outputs = %d, want both completed calls", historyOutputs)
	}
}

func TestPausedRunRetainsDefaultToolOutputSpill(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{{Items: []RunItem{
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "p1", Name: "present_plan", Input: json.RawMessage(`{}`)}},
	}}}}
	plan := &FunctionTool{ToolName: "present_plan", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("plan line\n", 500), nil
	}}
	result, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a", Tools: []Tool{plan}}, nil, RunConfig{MaxToolOutputBytes: 300, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var content string
	for _, item := range result.FinalHistory {
		if item.ToolOutput != nil {
			content = item.ToolOutput.Content
		}
	}
	const prefix = "[full output saved to "
	start := strings.Index(content, prefix)
	if start < 0 {
		t.Fatalf("missing spill hint in %q", content)
	}
	end := strings.Index(content[start:], "]") + start
	path := content[start+len(prefix) : end]
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill referenced by paused history was deleted: %v", err)
	}
}

func TestExecuteApprovedToolCheckpointsKeepResumedUsage(t *testing.T) {
	tool := &FunctionTool{ToolName: "t", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	var usages []Usage
	cfg := RunConfig{Durable: &DurableRunConfig{
		Resume: &DurableCheckpoint{SchemaVersion: DurableCheckpointSchemaVersion, Boundary: DurableBoundaryApprovalPending, Usage: Usage{InputTokens: 100, OutputTokens: 7}},
		Checkpoint: func(_ context.Context, cp DurableCheckpoint) error {
			usages = append(usages, cp.Usage)
			return nil
		},
	}}
	_, _, _, _, err := NewRunnerWithModel(&mockModel{}).ExecuteApprovedTool(context.Background(), &Agent{Name: "a", Tools: []Tool{tool}}, ToolCallData{ID: "1", Name: "t", Input: json.RawMessage(`{}`)}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 {
		t.Fatalf("checkpoints = %d, want 2", len(usages))
	}
	for _, u := range usages {
		if u.InputTokens != 100 || u.OutputTokens != 7 {
			t.Fatalf("checkpoint usage = %+v, want resumed usage preserved", u)
		}
	}
}

func TestInputItemsLogsSummaryAtNormalLevel(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "hi"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "1", Name: "t"}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "1"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "bye"}},
	}
	NewAgentLogger(LogLevelNormal).InputItems(items)
	out := strings.TrimSpace(buf.String())
	if strings.Count(out, "\n") != 0 {
		t.Fatalf("normal level logged %d lines, want 1:\n%s", strings.Count(out, "\n")+1, out)
	}
	if !strings.Contains(out, "input_items=4 message=2 tool_call=1 tool_output=1") {
		t.Fatalf("summary = %q", out)
	}
	buf.Reset()
	NewAgentLogger(LogLevelDebug).InputItems(items)
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; lines != 5 {
		t.Fatalf("debug level logged %d lines, want header + one per item", lines)
	}
}

func TestRequestSnapshotsOnlyForRealTracingAndNotRetained(t *testing.T) {
	newModel := func() *mockModel {
		return &mockModel{responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "t", Input: json.RawMessage(`{"a":1}`)}}}},
			finalResponse("done"),
		}}
	}
	tool := &FunctionTool{ToolName: "t", Schema: json.RawMessage(`{"type":"object"}`), ReadOnly: true, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	agent := &Agent{Name: "a", Tools: []Tool{tool}}

	rec := &spanRecorder{}
	trace := NewTrace("t")
	if _, err := NewRunnerWithModel(newModel()).Run(context.Background(), agent, nil, RunConfig{TracingProcessor: rec, Trace: trace}); err != nil {
		t.Fatal(err)
	}
	gens := rec.generations()
	if len(gens) != 2 || gens[1].Request == nil || gens[1].Response == nil {
		t.Fatalf("exported generations = %d, want request/response snapshots for the processor", len(gens))
	}
	if items := gens[1].Request.InputItems; len(items) != 2 || items[0].ToolCall == nil || string(items[0].ToolCall.Input) != `{"a":1}` {
		t.Fatalf("second request snapshot items = %+v", items)
	}
	if gens[1].TotalRequestTokenEstimate == 0 {
		t.Fatal("token estimate missing")
	}
	for _, s := range trace.Spans {
		if d, ok := s.Data.(*GenerationSpanData); ok && (d.Request != nil || d.Response != nil) {
			t.Fatal("trace.Spans retained a heavy request/response snapshot")
		}
	}

	trace = NewTrace("t")
	if _, err := NewRunnerWithModel(newModel()).Run(context.Background(), agent, nil, RunConfig{Trace: trace}); err != nil {
		t.Fatal(err)
	}
	for _, s := range trace.Spans {
		if d, ok := s.Data.(*GenerationSpanData); ok {
			if d.Request != nil {
				t.Fatal("request snapshot built without a tracing processor")
			}
			if d.TotalRequestTokenEstimate == 0 {
				t.Fatal("token estimate must still be recorded")
			}
		}
	}
}

func TestParentHistoryAttachedOnlyForSubAgentBatches(t *testing.T) {
	var plainSaw, delegateSaw atomic.Int32
	plain := &FunctionTool{ToolName: "plain", Schema: json.RawMessage(`{"type":"object"}`), ReadOnly: true, Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
		plainSaw.Store(int32(len(ParentRunItemsFromContext(ctx))))
		return "ok", nil
	}}
	delegate := &FunctionTool{ToolName: "subagent", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
		delegateSaw.Store(int32(len(ParentRunItemsFromContext(ctx))))
		return "ok", nil
	}}
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c1", Name: "plain", Input: json.RawMessage(`{}`)}}}},
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "c2", Name: "subagent", Input: json.RawMessage(`{}`)}}}},
		finalResponse("done"),
	}}
	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "task"}}}
	if _, err := NewRunnerWithModel(model).Run(context.Background(), &Agent{Name: "a", Tools: []Tool{plain, delegate}}, input, RunConfig{}); err != nil {
		t.Fatal(err)
	}
	if plainSaw.Load() != 0 {
		t.Fatalf("plain tool saw %d parent items, want none attached", plainSaw.Load())
	}
	if delegateSaw.Load() != 3 {
		t.Fatalf("subagent tool saw %d parent items, want completed history (3)", delegateSaw.Load())
	}
}
