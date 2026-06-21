package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/google/uuid"
)

const (
	defaultMaxConcurrent = 2 // max in-flight API requests across all goroutines
	maxRetries           = 3
	maxRetryAfterSeconds = 5 * 60
)

// Client wraps Anthropic API calls.
type Client struct {
	sdk sdk.Client

	sessionID    string
	sem          chan struct{} // concurrency limiter
	mu           sync.Mutex
	backoffUntil time.Time
}

// Option configures the Client.
type Option func(*clientConfig)

type clientConfig struct {
	baseURL       string
	maxConcurrent int
	authToken     string
	oauth         bool
	bearerToken   string
	headerProvider func(context.Context) (map[string]string, error)
}

// WithBaseURL overrides the API base URL.
func WithBaseURL(url string) Option {
	return func(c *clientConfig) { c.baseURL = url }
}

// WithMaxConcurrent sets the maximum number of in-flight API requests.
func WithMaxConcurrent(n int) Option {
	return func(c *clientConfig) { c.maxConcurrent = n }
}

// WithOAuthToken configures the client to authenticate with an Anthropic OAuth
// access token instead of an API key.
func WithOAuthToken(token string) Option {
	return func(c *clientConfig) {
		c.authToken = strings.TrimSpace(token)
		c.oauth = true
	}
}

// WithBearerToken authenticates with an "Authorization: Bearer <token>" header
// and, unlike WithOAuthToken, does NOT add the Anthropic OAuth beta header.
// It is intended for Anthropic-compatible gateways such as GitHub Copilot's
// /v1/messages endpoint, which expect a bearer token but reject Anthropic's
// first-party x-api-key / oauth headers.
func WithBearerToken(token string) Option {
	return func(c *clientConfig) {
		c.authToken = strings.TrimSpace(token)
		c.oauth = false
		c.bearerToken = strings.TrimSpace(token)
	}
}

// WithRequestHeaderProvider injects per-request headers via SDK middleware. The
// provider is invoked for every request, so callers can supply gateway auth and
// integration headers that may rotate between calls (mirrors the OpenAI custom
// auth session). Returned headers overwrite any same-named headers.
func WithRequestHeaderProvider(fn func(context.Context) (map[string]string, error)) Option {
	return func(c *clientConfig) { c.headerProvider = fn }
}

// NewClient creates a new Anthropic API client using an API key or OAuth
// access token.
func NewClient(apiKey string, opts ...Option) *Client {
	cfg := &clientConfig{maxConcurrent: defaultMaxConcurrent}
	for _, opt := range opts {
		opt(cfg)
	}

	sessionID := uuid.New().String()
	sdkOpts := []option.RequestOption{
		option.WithMaxRetries(0), // We handle retries ourselves
	}
	// x-app / X-Claude-Code-Session-Id are first-party Anthropic (Claude Code)
	// headers. Anthropic-compatible gateways such as GitHub Copilot don't expect
	// them, so only send them on the direct Anthropic API (api-key / oauth).
	if cfg.bearerToken == "" {
		sdkOpts = append(sdkOpts,
			option.WithHeader("x-app", "cli"),
			option.WithHeader("X-Claude-Code-Session-Id", sessionID),
		)
	}
	switch {
	case cfg.oauth:
		sdkOpts = append(sdkOpts,
			option.WithAuthToken(cfg.authToken),
			option.WithHeaderAdd("anthropic-beta", "oauth-2025-04-20"),
		)
	case cfg.bearerToken != "":
		// Bearer auth for Anthropic-compatible gateways (e.g. Copilot). No
		// x-api-key and no oauth beta header.
		sdkOpts = append(sdkOpts, option.WithAuthToken(cfg.bearerToken))
	default:
		sdkOpts = append(sdkOpts, option.WithAPIKey(apiKey))
	}
	if cfg.headerProvider != nil {
		sdkOpts = append(sdkOpts, option.WithMiddleware(headerProviderMiddleware(cfg.headerProvider)))
	}
	if cfg.baseURL != "" {
		sdkOpts = append(sdkOpts, option.WithBaseURL(cfg.baseURL))
	}

	sem := make(chan struct{}, defaultMaxConcurrent)
	if cfg.maxConcurrent > 0 {
		sem = make(chan struct{}, cfg.maxConcurrent)
	}

	return &Client{
		sdk:       sdk.NewClient(sdkOpts...),
		sessionID: sessionID,
		sem:       sem,
	}
}

// headerProviderMiddleware returns SDK middleware that overwrites request
// headers with the values supplied by provider on every call.
func headerProviderMiddleware(provider func(context.Context) (map[string]string, error)) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		headers, err := provider(req.Context())
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return next(req)
	}
}

// CreateMessage sends a non-streaming request to the Messages API.
func (c *Client) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
	return c.createMessageSDK(ctx, req)
}

// CreateMessageStream sends a streaming request and returns a StreamReader.
func (c *Client) CreateMessageStream(ctx context.Context, req CreateMessageRequest) (*StreamReader, error) {
	return c.createMessageStreamSDK(ctx, req)
}

// ---- SDK-based methods (API key auth) ----

func (c *Client) createMessageSDK(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
	params, betas := toSDKParams(&req)

	var resp *CreateMessageResponse
	err := c.doWithRetry(ctx, func(ctx context.Context) error {
		msg, err := c.sdk.Beta.Messages.New(ctx, params, betas...)
		if err != nil {
			return err
		}
		resp = fromSDKBetaMessage(msg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) createMessageStreamSDK(ctx context.Context, req CreateMessageRequest) (*StreamReader, error) {
	params, betas := toSDKParams(&req)

	var reader *StreamReader
	err := c.doWithRetry(ctx, func(ctx context.Context) error {
		stream := c.sdk.Beta.Messages.NewStreaming(ctx, params, betas...)
		reader = &StreamReader{sdkStream: stream}
		if stream.Err() != nil {
			return stream.Err()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reader, nil
}

// ---- Shared retry logic ----

// RequestError represents an API error with status code.
type RequestError struct {
	StatusCode     int
	Body           string
	retryAfter     int
	shouldRetry    *bool
	RateLimitReset string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("API request failed with status %d: %s", e.StatusCode, e.Body)
}

// Retryable returns true if the error is retryable.
func (e *RequestError) Retryable() bool {
	if e.shouldRetry != nil && !*e.shouldRetry {
		return false
	}
	if e.StatusCode == 429 {
		return true
	}
	return e.StatusCode == 529 || e.StatusCode >= 500
}

// RetryAfterMS returns the retry-after value in milliseconds.
func (e *RequestError) RetryAfterMS() int {
	return capRetryAfterSeconds(e.retryAfter) * 1000
}

// waitForBackoff blocks until any global backoff expires or ctx is cancelled.
func (c *Client) waitForBackoff(ctx context.Context) error {
	c.mu.Lock()
	until := c.backoffUntil
	c.mu.Unlock()

	if delay := time.Until(until); delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil
}

// setGlobalBackoff sets a shared backoff deadline.
func (c *Client) setGlobalBackoff(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(c.backoffUntil) {
		c.backoffUntil = until
	}
}

// doWithRetry wraps an API call with retry logic for transient errors.
func (c *Client) doWithRetry(ctx context.Context, fn func(ctx context.Context) error) error {
	retryAfterUsed := false
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 && !retryAfterUsed {
			baseDelay := time.Duration(1<<uint(attempt-1)) * time.Second
			jitter := time.Duration(rand.Int63n(int64(baseDelay / 2)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(baseDelay + jitter):
			}
		}
		retryAfterUsed = false

		if err := c.waitForBackoff(ctx); err != nil {
			return err
		}

		// Acquire concurrency semaphore.
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}

		err := fn(ctx)
		<-c.sem

		if err == nil {
			return nil
		}

		// Convert SDK error to our RequestError (for SDK path).
		reqErr := toRequestError(err)
		if reqErr == nil {
			// Not an API error — network error etc.
			if attempt < maxRetries {
				log.Printf("[anthropic] Non-API error (attempt %d/%d): %v", attempt+1, maxRetries+1, err)
				continue
			}
			return err
		}

		log.Printf("[anthropic] HTTP %d (attempt %d/%d): %s", reqErr.StatusCode, attempt+1, maxRetries+1, reqErr.Body)

		if !reqErr.Retryable() || attempt >= maxRetries {
			return reqErr
		}

		retryAfter := capRetryAfterSeconds(reqErr.retryAfter)
		if reqErr.StatusCode == 429 && retryAfter > 0 {
			c.setGlobalBackoff(time.Duration(retryAfter) * time.Second)
		}

		if retryAfter > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(retryAfter) * time.Second):
			}
			retryAfterUsed = true
		}
	}

	return fmt.Errorf("max retries exceeded")
}

func capRetryAfterSeconds(seconds int) int {
	if seconds <= 0 {
		return 0
	}
	if seconds > maxRetryAfterSeconds {
		return maxRetryAfterSeconds
	}
	return seconds
}

// toRequestError converts an error to our RequestError, or nil if not an API error.
func toRequestError(err error) *RequestError {
	// Check if it's already our RequestError.
	var reqErr *RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	// Check if it's an SDK error.
	var sdkErr *sdk.Error
	if errors.As(err, &sdkErr) {
		reqErr := &RequestError{
			StatusCode: sdkErr.StatusCode,
			Body:       string(sdkErr.DumpResponse(true)),
		}
		if sdkErr.Response != nil {
			reqErr.retryAfter = parseRetryAfterSeconds(sdkErr.Response.Header)
			reqErr.RateLimitReset = strings.TrimSpace(sdkErr.Response.Header.Get("anthropic-ratelimit-requests-reset"))
		}
		return reqErr
	}

	return nil
}

// parseRetryAfterSeconds extracts a retry delay in seconds from response
// headers. It honors the standard Retry-After header (delta-seconds or an
// HTTP date) so the client respects provider-directed backoff on 429/503.
func parseRetryAfterSeconds(h http.Header) int {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0
		}
		return secs
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := int(time.Until(t).Seconds()); d > 0 {
			return d
		}
	}
	return 0
}

// ---- StreamReader ----

// StreamReader wraps an SDK stream.
type StreamReader struct {
	sdkStream *ssestream.Stream[sdk.BetaRawMessageStreamEventUnion]
}

// Next returns the next StreamEvent. Returns io.EOF when the stream ends.
func (r *StreamReader) Next() (StreamEvent, error) {
	return r.nextSDK()
}

func (r *StreamReader) nextSDK() (StreamEvent, error) {
	for r.sdkStream.Next() {
		sdkEvent := r.sdkStream.Current()
		event := fromSDKStreamEvent(sdkEvent)
		if event == nil {
			continue
		}
		return *event, nil
	}

	if err := r.sdkStream.Err(); err != nil {
		return StreamEvent{}, err
	}
	return StreamEvent{Type: EventMessageStop}, io.EOF
}

// Close closes the underlying stream.
func (r *StreamReader) Close() error {
	if r.sdkStream == nil {
		return nil
	}
	return r.sdkStream.Close()
}

// ---- SDK stream event converters (API key path only) ----

// fromSDKStreamEvent converts an SDK stream event to our StreamEvent.
func fromSDKStreamEvent(u sdk.BetaRawMessageStreamEventUnion) *StreamEvent {
	switch u.Type {
	case "message_start":
		e := u.AsMessageStart()
		msg := fromSDKBetaMessage(&e.Message)
		return &StreamEvent{
			Type:    EventMessageStart,
			Message: msg,
		}
	case "content_block_start":
		e := u.AsContentBlockStart()
		block := fromSDKContentBlockStart(e)
		return &StreamEvent{
			Type:         EventContentBlockStart,
			Index:        int(e.Index),
			ContentBlock: block,
		}
	case "content_block_delta":
		e := u.AsContentBlockDelta()
		delta := fromSDKDelta(e)
		return &StreamEvent{
			Type:  EventContentBlockDelta,
			Index: int(e.Index),
			Delta: delta,
		}
	case "content_block_stop":
		e := u.AsContentBlockStop()
		return &StreamEvent{
			Type:  EventContentBlockStop,
			Index: int(e.Index),
		}
	case "message_delta":
		e := u.AsMessageDelta()
		return &StreamEvent{
			Type: EventMessageDelta,
			Delta: &DeltaBlock{
				Type:       "message_delta",
				StopReason: string(e.Delta.StopReason),
			},
			Usage: &Usage{
				OutputTokens: int64(e.Usage.OutputTokens),
			},
		}
	case "message_stop":
		return &StreamEvent{Type: EventMessageStop}
	case "ping":
		return nil
	case "error":
		return nil
	default:
		return nil
	}
}

// fromSDKBetaMessage converts an SDK BetaMessage to our CreateMessageResponse.
func fromSDKBetaMessage(msg *sdk.BetaMessage) *CreateMessageResponse {
	resp := &CreateMessageResponse{
		ID:         msg.ID,
		Type:       "message",
		Role:       Role(msg.Role),
		Model:      msg.Model,
		StopReason: StopReason(msg.StopReason),
		Usage: Usage{
			InputTokens:              int64(msg.Usage.InputTokens),
			OutputTokens:             int64(msg.Usage.OutputTokens),
			CacheReadInputTokens:     int64(msg.Usage.CacheReadInputTokens),
			CacheCreationInputTokens: int64(msg.Usage.CacheCreationInputTokens),
		},
	}

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			b := block.AsText()
			resp.Content = append(resp.Content, NewTextBlock(b.Text))
		case "thinking":
			b := block.AsThinking()
			thinking := NewThinkingBlock(b.Thinking)
			thinking.Signature = b.Signature
			resp.Content = append(resp.Content, thinking)
		case "redacted_thinking":
			b := block.AsRedactedThinking()
			resp.Content = append(resp.Content, NewRedactedThinkingBlock(b.Data))
		case "tool_use":
			b := block.AsToolUse()
			input, _ := json.Marshal(b.Input)
			resp.Content = append(resp.Content, NewToolUseBlock(b.ID, b.Name, input))
		}
	}

	return resp
}

// fromSDKContentBlockStart converts a content_block_start event.
func fromSDKContentBlockStart(e sdk.BetaRawContentBlockStartEvent) *ContentBlock {
	switch e.ContentBlock.Type {
	case "text":
		b := e.ContentBlock.AsText()
		return &ContentBlock{Type: "text", Text: b.Text}
	case "thinking":
		b := e.ContentBlock.AsThinking()
		return &ContentBlock{Type: "thinking", Thinking: b.Thinking, Signature: b.Signature}
	case "redacted_thinking":
		b := e.ContentBlock.AsRedactedThinking()
		return &ContentBlock{Type: "redacted_thinking", Data: b.Data}
	case "tool_use":
		b := e.ContentBlock.AsToolUse()
		return &ContentBlock{Type: "tool_use", ID: b.ID, Name: b.Name}
	default:
		return &ContentBlock{Type: string(e.ContentBlock.Type)}
	}
}

// fromSDKDelta converts a content_block_delta event.
func fromSDKDelta(e sdk.BetaRawContentBlockDeltaEvent) *DeltaBlock {
	switch e.Delta.Type {
	case "text_delta":
		d := e.Delta.AsTextDelta()
		return &DeltaBlock{Type: "text_delta", Text: d.Text}
	case "thinking_delta":
		d := e.Delta.AsThinkingDelta()
		return &DeltaBlock{Type: "thinking_delta", Thinking: d.Thinking}
	case "signature_delta":
		d := e.Delta.AsSignatureDelta()
		return &DeltaBlock{Type: "signature_delta", Signature: d.Signature}
	case "input_json_delta":
		d := e.Delta.AsInputJSONDelta()
		return &DeltaBlock{Type: "input_json_delta", PartialJSON: d.PartialJSON}
	default:
		return &DeltaBlock{Type: string(e.Delta.Type)}
	}
}
