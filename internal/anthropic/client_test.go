package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClient_Defaults(t *testing.T) {
	c := NewClient("test-key")
	if c == nil {
		t.Fatalf("NewClient returned nil")
	}
	if c.sem == nil {
		t.Fatalf("NewClient did not initialize semaphore")
	}
}

func TestNewClient_OAuthHeaders(t *testing.T) {
	var gotAuth, gotAPIKey, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_test",
			"type":"message",
			"role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-sonnet-4-5",
			"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	c := NewClient("unused-api-key", WithBaseURL(srv.URL), WithOAuthToken("oauth-token"))
	_, err := c.CreateMessage(context.Background(), CreateMessageRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1,
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{NewTextBlock("hello")},
		}},
	})
	if err != nil {
		t.Fatalf("CreateMessage() error = %v", err)
	}
	if gotAuth != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want Bearer oauth-token", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("x-api-key = %q, want empty", gotAPIKey)
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Fatalf("anthropic-beta = %q, want oauth-2025-04-20", gotBeta)
	}
}

func TestRequestError_Retryable(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{529, true},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			err := &RequestError{StatusCode: tt.code}
			if got := err.Retryable(); got != tt.want {
				t.Errorf("Retryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequestErrorRetryAfterMSCapsHugeProviderDelay(t *testing.T) {
	err := &RequestError{retryAfter: maxRetryAfterSeconds + 3600}
	if got, want := err.RetryAfterMS(), maxRetryAfterSeconds*1000; got != want {
		t.Fatalf("RetryAfterMS() = %d, want %d", got, want)
	}
}
