// Package errors_retries_test exercises retry-on-error behavior. Live
// providers cannot be made to return controlled errors deterministically, so
// this file uses a tiny in-test fake model to drive the SDK's retry plumbing.
// The retry-policy math itself is provider-agnostic, so a fake model is the
// correct level of abstraction for the example.
package errors_retries_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

type retryFakeModel struct {
	mu          sync.Mutex
	calls       int
	errors      []error
	responses   []*agentsdk.ModelResponse
	retryAdvice *agentsdk.ModelRetryAdvice
}

func (m *retryFakeModel) Provider() string                       { return "fake" }
func (m *retryFakeModel) CalculateCost(_ agentsdk.Usage) float64 { return 0 }

func (m *retryFakeModel) GetRetryAdvice(err error) *agentsdk.ModelRetryAdvice {
	if err == nil {
		return nil
	}
	if m.retryAdvice != nil {
		advice := *m.retryAdvice
		return &advice
	}
	return &agentsdk.ModelRetryAdvice{ShouldRetry: true}
}

func (m *retryFakeModel) GetResponse(_ context.Context, _ agentsdk.ModelRequest) (*agentsdk.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.calls
	m.calls++
	if idx < len(m.errors) && m.errors[idx] != nil {
		return nil, m.errors[idx]
	}
	if idx < len(m.responses) && m.responses[idx] != nil {
		return m.responses[idx], nil
	}
	return m.responses[len(m.responses)-1], nil
}

func (m *retryFakeModel) StreamResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelStream, error) {
	resp, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan agentsdk.ModelStreamEvent, 1)
	events <- agentsdk.ModelStreamEvent{Type: agentsdk.ModelStreamComplete, Response: resp}
	close(events)
	done := make(chan *agentsdk.ModelResponse, 1)
	done <- resp
	close(done)
	return agentsdk.NewModelStream(events, done), nil
}

func textResp(text string) *agentsdk.ModelResponse {
	return &agentsdk.ModelResponse{
		Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: text}}},
	}
}

func TestProviderRetryAdviceExample(t *testing.T) {
	transient := errors.New("temporary provider outage")
	model := &retryFakeModel{
		errors:      []error{transient, nil},
		responses:   []*agentsdk.ModelResponse{nil, textResp("recovered")},
		retryAdvice: &agentsdk.ModelRetryAdvice{ShouldRetry: true, RetryAfterMS: 0, Reason: "temporary"},
	}
	runner := agentsdk.NewRunnerWithModel(model)
	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "retrying",
		Model:        "fake",
		Instructions: "Recover from transient errors.",
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Try once, then recover."},
		},
	}, agentsdk.RunConfig{MaxTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "recovered" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
	if model.calls != 2 {
		t.Fatalf("calls = %d, want 2", model.calls)
	}
}

func TestStandaloneRetryPolicyExample(t *testing.T) {
	policy := agentsdk.DefaultRetryPolicy()
	if policy.MaxRetries != 3 {
		t.Fatalf("MaxRetries = %d", policy.MaxRetries)
	}
	if got := policy.DelayForAttempt(2); got != 4*time.Second {
		t.Fatalf("DelayForAttempt(2) = %s, want 4s", got)
	}
}

func TestRunConfigRetryPolicyExample(t *testing.T) {
	model := &retryFakeModel{
		errors:    []error{errors.New("temporary"), nil},
		responses: []*agentsdk.ModelResponse{nil, textResp("policy recovered")},
	}
	runner := agentsdk.NewRunnerWithModel(model)
	policy := agentsdk.DefaultRetryPolicy()
	policy.MaxRetries = 1
	policy.Backoff.InitialDelayMS = 0
	policy.Backoff.MaxDelayMS = 0

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:  "retry-policy",
		Model: "fake",
	}, nil, agentsdk.RunConfig{
		MaxTurns:    2,
		RetryPolicy: &policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText() != "policy recovered" {
		t.Fatalf("FinalText() = %q", result.FinalText())
	}
}
