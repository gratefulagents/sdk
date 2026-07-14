package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/internal/modelactivity"
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

func TestCreateMessageReportsStreamingActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_activity","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n")))
	}))
	defer srv.Close()

	var activityCount atomic.Int32
	ctx := modelactivity.WithSink(context.Background(), func() { activityCount.Add(1) })
	client := NewClient("test-key", WithBaseURL(srv.URL))
	resp, err := client.CreateMessage(ctx, CreateMessageRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 8,
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{NewTextBlock("hello")},
		}},
	})
	if err != nil {
		t.Fatalf("CreateMessage() error = %v", err)
	}
	if activityCount.Load() == 0 {
		t.Fatal("Anthropic stream did not report model activity")
	}
	if resp == nil || len(resp.Content) != 1 || resp.Content[0].Text != "ok" {
		t.Fatalf("CreateMessage() response = %+v, want text ok", resp)
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

func TestApplyRateLimitHeaders_UnifiedReset(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "rejected")
	h.Set("anthropic-ratelimit-unified-reset", strconvItoa64(time.Now().Add(2*time.Minute).Unix()))
	e := &RequestError{StatusCode: 429}
	applyRateLimitHeaders(e, h)

	if e.UnifiedStatus != "rejected" {
		t.Fatalf("UnifiedStatus = %q, want rejected", e.UnifiedStatus)
	}
	got := e.RetryAfterMS()
	if got < 100*1000 || got > 121*1000 {
		t.Fatalf("RetryAfterMS() = %d, want ~120000", got)
	}
	if !strings.Contains(e.Error(), "unified-status rejected") {
		t.Fatalf("Error() = %q, want unified-status hint", e.Error())
	}
}

func TestApplyRateLimitHeaders_RetryAfterWinsOverReset(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	h.Set("anthropic-ratelimit-requests-reset", time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	e := &RequestError{StatusCode: 429}
	applyRateLimitHeaders(e, h)
	if got := e.RetryAfterMS(); got != 7000 {
		t.Fatalf("RetryAfterMS() = %d, want 7000", got)
	}
}

func TestApplyRateLimitHeaders_XShouldRetryFalse(t *testing.T) {
	h := http.Header{}
	h.Set("X-Should-Retry", "false")
	e := &RequestError{StatusCode: 429}
	applyRateLimitHeaders(e, h)
	if e.Retryable() {
		t.Fatalf("Retryable() = true, want false when X-Should-Retry is false")
	}

	h2 := http.Header{}
	h2.Set("X-Should-Retry", "true")
	e2 := &RequestError{StatusCode: 408}
	applyRateLimitHeaders(e2, h2)
	if !e2.Retryable() {
		t.Fatalf("Retryable() = false, want true when X-Should-Retry is true")
	}
}

func TestRateLimitBackoffBounds(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		d := rateLimitBackoff(attempt)
		if d < rateLimitBaseBackoff {
			t.Fatalf("attempt %d: backoff %v below base %v", attempt, d, rateLimitBaseBackoff)
		}
		if d > rateLimitMaxBackoff+rateLimitMaxBackoff/2+time.Second {
			t.Fatalf("attempt %d: backoff %v above cap", attempt, d)
		}
	}
	if d := rateLimitBackoff(-3); d < rateLimitBaseBackoff {
		t.Fatalf("negative attempt backoff %v below base", d)
	}
}

type staticTokenSource struct {
	mu          sync.Mutex
	token       string
	invalidated int
}

func (s *staticTokenSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, nil
}

func (s *staticTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
	s.token = "refreshed-token"
}

func TestClient_TokenSourceRefreshesOn401(t *testing.T) {
	var calls int32
	var authHeaders []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_test","type":"message","role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-sonnet-4-5","stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	source := &staticTokenSource{token: "stale-token"}
	c := NewClient("", WithBaseURL(srv.URL), WithOAuthTokenSource(source))
	_, err := c.CreateMessage(context.Background(), CreateMessageRequest{
		Model: "claude-sonnet-4-5", MaxTokens: 1,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{NewTextBlock("hi")}}},
	})
	if err != nil {
		t.Fatalf("CreateMessage() error = %v", err)
	}
	if source.invalidated != 1 {
		t.Fatalf("invalidated = %d, want 1", source.invalidated)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authHeaders) != 2 || authHeaders[0] != "Bearer stale-token" || authHeaders[1] != "Bearer refreshed-token" {
		t.Fatalf("auth headers = %v, want stale then refreshed", authHeaders)
	}
}

func TestNewClient_OAuthClaudeCodeShaping(t *testing.T) {
	var gotUA, gotDirect string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotDirect = r.Header.Get("anthropic-dangerous-direct-browser-access")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_test","type":"message","role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-sonnet-4-5","stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	c := NewClient("unused", WithBaseURL(srv.URL), WithOAuthToken("oauth-token"))
	_, err := c.CreateMessage(context.Background(), CreateMessageRequest{
		Model: "claude-sonnet-4-5", MaxTokens: 1,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{NewTextBlock("hi")}}},
	})
	if err != nil {
		t.Fatalf("CreateMessage() error = %v", err)
	}
	if gotUA != oauthUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, oauthUserAgent)
	}
	if gotDirect != "true" {
		t.Fatalf("anthropic-dangerous-direct-browser-access = %q, want true", gotDirect)
	}
}

func strconvItoa64(v int64) string { return strconv.FormatInt(v, 10) }
