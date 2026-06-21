package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fallbackTestProvider struct {
	models map[string]*fallbackTestModel
	names  []string
}

func (p *fallbackTestProvider) GetModel(name string) (Model, error) {
	p.names = append(p.names, name)
	model := p.models[name]
	if model == nil {
		return nil, errors.New("unknown model " + name)
	}
	return model, nil
}

func (p *fallbackTestProvider) Close() error { return nil }

type fallbackTestModel struct {
	provider     string
	responses    []*ModelResponse
	errors       []error
	retryAdvices []ModelRetryAdvice
	requests     []ModelRequest
	callIdx      int
}

func (m *fallbackTestModel) GetResponse(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.requests = append(m.requests, req)
	idx := m.callIdx
	m.callIdx++
	if idx < len(m.errors) && m.errors[idx] != nil {
		return nil, m.errors[idx]
	}
	if idx >= len(m.responses) {
		return nil, errors.New("no response")
	}
	return m.responses[idx], nil
}

func (m *fallbackTestModel) StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error) {
	resp, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan ModelStreamEvent, 1)
	done := make(chan *ModelResponse, 1)
	go func() {
		defer close(events)
		events <- ModelStreamEvent{Type: ModelStreamComplete, Response: resp}
		done <- resp
	}()
	return NewModelStream(events, done), nil
}

func (m *fallbackTestModel) GetRetryAdvice(error) *ModelRetryAdvice {
	idx := m.callIdx - 1
	if idx >= 0 && idx < len(m.retryAdvices) {
		advice := m.retryAdvices[idx]
		return &advice
	}
	return &ModelRetryAdvice{ShouldRetry: false}
}

func (m *fallbackTestModel) CalculateCost(Usage) float64 { return 0 }
func (m *fallbackTestModel) Provider() string            { return m.provider }

func TestRunnerFallsBackAcrossProvidersOnRateLimit(t *testing.T) {
	rateLimitErr := errors.New("subscription limit reached")
	primary := &fallbackTestModel{
		provider:     "openai",
		errors:       []error{rateLimitErr},
		retryAdvices: []ModelRetryAdvice{{ShouldRetry: true, Reason: "429"}},
	}
	fallback := &fallbackTestModel{
		provider: "anthropic",
		responses: []*ModelResponse{{
			Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done on fallback"}}},
		}},
	}
	provider := &fallbackTestProvider{models: map[string]*fallbackTestModel{
		"openai/gpt-primary":          primary,
		"anthropic/claude-sonnet-4-6": fallback,
	}}
	runner := NewRunnerWithProvider(provider)
	agent := &Agent{
		Name:           "agent",
		Model:          "openai/gpt-primary",
		FallbackModels: []string{"anthropic/claude-sonnet-4-6", "copilot/gpt-4.1"},
	}

	var buf bytes.Buffer
	hooks := NewPlatformHooks(NewProgressTracker(), NewEventStream(&buf))
	result, err := runner.Run(context.Background(), agent, nil, RunConfig{Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "done on fallback" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
	if strings.Join(provider.names, ",") != "openai/gpt-primary,anthropic/claude-sonnet-4-6" {
		t.Fatalf("provider names = %#v", provider.names)
	}
	if got := fallback.requests[0].Model; got != "claude-sonnet-4-6" {
		t.Fatalf("fallback request model = %q, want claude-sonnet-4-6", got)
	}

	attempts := decodeLLMAttempts(t, buf.String())
	if len(attempts) != 4 {
		t.Fatalf("llm attempts = %d, want 4: %+v", len(attempts), attempts)
	}
	if attempts[1].Status != "fallback" || !attempts[1].FallbackPlanned || attempts[1].FallbackFromModel != "openai/gpt-primary" || attempts[1].FallbackToModel != "anthropic/claude-sonnet-4-6" || attempts[1].FallbackReason != "rate_limit" {
		t.Fatalf("fallback event = %+v", attempts[1])
	}
	if attempts[2].RequestedModel != "anthropic/claude-sonnet-4-6" || attempts[2].ResolvedModel != "claude-sonnet-4-6" || attempts[2].Provider != "anthropic" {
		t.Fatalf("fallback start event = %+v", attempts[2])
	}
}

func TestShouldFallbackModelCallDefaultsToRateLimitOnly(t *testing.T) {
	if !shouldFallbackModelCall(errors.New("limited"), &ModelRetryAdvice{ShouldRetry: true, Reason: "429"}) {
		t.Fatal("429 should be fallback eligible")
	}
	if shouldFallbackModelCall(errors.New("server down"), &ModelRetryAdvice{ShouldRetry: true, Reason: "500"}) {
		t.Fatal("500 should stay on the normal retry path by default")
	}
}

func TestShouldFallbackModelCallRejectsCommittedStreamOutput(t *testing.T) {
	err := &streamOutputCommittedError{cause: errors.New("rate limit after delta")}
	if shouldFallbackModelCall(err, &ModelRetryAdvice{ShouldRetry: true, Reason: "429"}) {
		t.Fatal("stream errors after emitted output must not fallback")
	}
}

func decodeLLMAttempts(t *testing.T, data string) []ContentEvent {
	t.Helper()
	var attempts []ContentEvent
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev ContentEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "llm_attempt" {
			attempts = append(attempts, ev)
		}
	}
	return attempts
}
