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

	"github.com/gratefulagents/sdk/internal/modeldelta"
)

const (
	defaultMaxConcurrent = 2 // max in-flight API requests across all goroutines
	maxRetries           = 3
	maxRetryAfterSeconds = 5 * 60

	// oauthUserAgent identifies OAuth (Claude subscription) requests the way
	// Claude Code does. Anthropic's OAuth endpoints expect Claude Code-shaped
	// traffic; other OAuth integrations (pi-anthropic-oauth, opencode) send an
	// equivalent identity for compatibility.
	oauthUserAgent = "claude-cli/2.1.158 (external, cli)"

	// rateLimitBaseBackoff is the first-retry delay for 429/529 responses that
	// carry no Retry-After or rate-limit reset headers. Rate limits recover on
	// the provider's clock, not ours, so hammering with sub-second retries
	// only burns attempts; 5s doubling mirrors pi-anthropic-oauth.
	rateLimitBaseBackoff = 5 * time.Second
	// rateLimitMaxBackoff caps the headerless rate-limit backoff.
	rateLimitMaxBackoff = 60 * time.Second
)

// TokenSource supplies OAuth bearer tokens per request so long-lived clients
// pick up rotated/refreshed credentials instead of pinning the token captured
// at construction time.
type TokenSource interface {
	// Token returns the current access token.
	Token(ctx context.Context) (string, error)
	// Invalidate marks the cached token as suspect (e.g. after a 401) so the
	// next Token call re-reads or refreshes the underlying credential.
	Invalidate()
}

// Client wraps Anthropic API calls.
type Client struct {
	sdk sdk.Client

	sessionID    string
	sem          chan struct{} // concurrency limiter
	tokenSource  TokenSource
	mu           sync.Mutex
	backoffUntil time.Time
}

// Option configures the Client.
type Option func(*clientConfig)

type clientConfig struct {
	baseURL        string
	maxConcurrent  int
	authToken      string
	oauth          bool
	bearerToken    string
	tokenSource    TokenSource
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

// WithOAuthTokenSource configures OAuth auth like WithOAuthToken but resolves
// the bearer token per request from source, so rotated or self-refreshed
// credentials take effect mid-run. On a 401 the client invalidates the source
// and retries once with a fresh token.
func WithOAuthTokenSource(source TokenSource) Option {
	return func(c *clientConfig) {
		if source == nil {
			return
		}
		c.oauth = true
		c.tokenSource = source
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
			option.WithHeaderAdd("anthropic-beta", "oauth-2025-04-20"),
			// Claude Code-compatible OAuth shaping: subscription OAuth expects
			// Claude Code-shaped traffic. See pi-anthropic-oauth / opencode.
			option.WithHeader("User-Agent", oauthUserAgent),
			option.WithHeader("anthropic-dangerous-direct-browser-access", "true"),
		)
		if cfg.tokenSource != nil {
			// Per-request bearer resolution so refreshed/rotated tokens take
			// effect mid-run. A static fallback keeps requests authenticated
			// if the source starts failing after construction.
			if cfg.authToken != "" {
				sdkOpts = append(sdkOpts, option.WithAuthToken(cfg.authToken))
			}
			sdkOpts = append(sdkOpts, option.WithMiddleware(tokenSourceMiddleware(cfg.tokenSource)))
		} else {
			sdkOpts = append(sdkOpts, option.WithAuthToken(cfg.authToken))
		}
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
		sdk:         sdk.NewClient(sdkOpts...),
		sessionID:   sessionID,
		sem:         sem,
		tokenSource: cfg.tokenSource,
	}
}

// tokenSourceMiddleware resolves the OAuth bearer token per request.
func tokenSourceMiddleware(source TokenSource) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		token, err := source.Token(req.Context())
		if err != nil {
			return nil, fmt.Errorf("resolve anthropic oauth token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return next(req)
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

// CreateMessage sends a request to the Messages API and returns the complete
// assembled response. It streams under the hood (accumulating into a single
// message) so it never trips the SDK's non-streaming guard, which rejects plain
// requests whose max_tokens could take longer than 10 minutes — the failure
// observed for sub-agents on Copilot's /v1/messages shim.
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
		// Stream and accumulate rather than calling the blocking endpoint:
		// Beta.Messages.New refuses requests whose max_tokens could exceed the
		// 10-minute non-streaming limit. Streaming has no such cap, and the
		// SDK's own accumulator reassembles the identical final message.
		sink := modeldelta.ReasoningSinkFromContext(ctx)
		stream := c.sdk.Beta.Messages.NewStreaming(ctx, params, betas...)
		defer stream.Close()
		var acc sdk.BetaMessage
		for stream.Next() {
			event := stream.Current()
			if err := acc.Accumulate(event); err != nil {
				return err
			}
			// Surface reasoning text live to any installed sink while the
			// blocking call keeps accumulating the full response.
			if sink != nil && event.Type == "content_block_delta" {
				if delta := event.AsContentBlockDelta().Delta; delta.Type == "thinking_delta" {
					if text := delta.AsThinkingDelta().Thinking; text != "" {
						sink(text)
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		resp = fromSDKBetaMessage(&acc)
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
		if err := stream.Err(); err != nil {
			// Close the failed stream so its response body is not leaked
			// across retries.
			_ = stream.Close()
			return err
		}
		reader = &StreamReader{sdkStream: stream}
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
	StatusCode  int
	Body        string
	retryAfter  int
	shouldRetry *bool
	// RateLimitReset is the raw anthropic-ratelimit-requests-reset header
	// (RFC 3339), kept for callers that want the provider's request-bucket
	// reset time.
	RateLimitReset string
	// UnifiedStatus is the anthropic-ratelimit-unified-status header sent on
	// OAuth (Claude subscription) traffic: e.g. "allowed", "allowed_warning",
	// "rejected". "rejected" means the subscription usage window is exhausted.
	UnifiedStatus string
	// resetAt is the earliest known rate-limit reset instant derived from
	// anthropic-ratelimit-unified-reset / -requests-reset / -tokens-reset.
	resetAt time.Time
	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

func (e *RequestError) Error() string {
	msg := fmt.Sprintf("API request failed with status %d", e.StatusCode)
	if hint := e.rateLimitHint(); hint != "" {
		msg += " (" + hint + ")"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// rateLimitHint renders a concise human-readable summary of any rate-limit
// headers so logs and run errors say when the limit clears instead of dumping
// raw headers.
func (e *RequestError) rateLimitHint() string {
	var parts []string
	if e.UnifiedStatus != "" {
		parts = append(parts, "unified-status "+e.UnifiedStatus)
	}
	if ms := e.retryAfterMSUncapped(); ms > 0 {
		parts = append(parts, "rate limit resets in "+time.Duration(ms*int64(time.Millisecond)).Round(time.Second).String())
	}
	return strings.Join(parts, ", ")
}

// Retryable returns true if the error is retryable.
func (e *RequestError) Retryable() bool {
	if e.shouldRetry != nil && !*e.shouldRetry {
		return false
	}
	if e.StatusCode == 429 {
		return true
	}
	if e.shouldRetry != nil && *e.shouldRetry {
		return true
	}
	return e.StatusCode == 529 || e.StatusCode >= 500
}

// IsRateLimit reports whether the error is a rate-limit (429) response.
func (e *RequestError) IsRateLimit() bool { return e.StatusCode == 429 }

// retryAfterMSUncapped derives the provider-directed retry delay: an explicit
// Retry-After wins, otherwise the earliest rate-limit reset timestamp.
func (e *RequestError) retryAfterMSUncapped() int64 {
	if e.retryAfter > 0 {
		return int64(e.retryAfter) * 1000
	}
	if !e.resetAt.IsZero() {
		nowFn := e.now
		if nowFn == nil {
			nowFn = time.Now
		}
		if d := e.resetAt.Sub(nowFn()); d > 0 {
			return d.Milliseconds()
		}
	}
	return 0
}

// RetryAfterMS returns the provider-directed retry delay in milliseconds,
// capped at maxRetryAfterSeconds. Zero means the provider gave no guidance.
func (e *RequestError) RetryAfterMS() int {
	ms := e.retryAfterMSUncapped()
	if ms <= 0 {
		return 0
	}
	if capMS := int64(maxRetryAfterSeconds) * 1000; ms > capMS {
		return int(capMS)
	}
	return int(ms)
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
	sleptForRetry := false
	tokenRefreshed := false
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 && !sleptForRetry {
			baseDelay := time.Duration(1<<uint(attempt-1)) * time.Second
			jitter := time.Duration(rand.Int63n(int64(baseDelay / 2)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(baseDelay + jitter):
			}
		}
		sleptForRetry = false

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

		log.Printf("[anthropic] HTTP %d (attempt %d/%d): %s", reqErr.StatusCode, attempt+1, maxRetries+1, errorLogBody(reqErr))

		// An auth failure with a refreshable token source usually means the
		// in-memory token was rotated or expired mid-run: invalidate once and
		// retry immediately with a freshly resolved token.
		if reqErr.StatusCode == http.StatusUnauthorized && c.tokenSource != nil && !tokenRefreshed {
			tokenRefreshed = true
			c.tokenSource.Invalidate()
			sleptForRetry = true // no backoff needed; the retry uses a new credential
			continue
		}

		if !reqErr.Retryable() || attempt >= maxRetries {
			return reqErr
		}

		// Prefer provider-directed delay (Retry-After or rate-limit reset
		// headers). A headerless 429/529 still gets a rate-limit-scale
		// backoff: those limits recover on the provider's clock, so the
		// generic 1s ladder just wastes attempts.
		delay := time.Duration(reqErr.RetryAfterMS()) * time.Millisecond
		if delay == 0 && (reqErr.StatusCode == 429 || reqErr.StatusCode == 529) {
			delay = rateLimitBackoff(attempt)
		}
		if reqErr.StatusCode == 429 && delay > 0 {
			c.setGlobalBackoff(delay)
		}

		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			sleptForRetry = true
		}
	}

	return fmt.Errorf("max retries exceeded")
}

// rateLimitBackoff returns the headerless 429/529 backoff for a 0-indexed
// attempt: rateLimitBaseBackoff doubling per attempt plus up to 50% jitter,
// capped at rateLimitMaxBackoff.
func rateLimitBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := rateLimitBaseBackoff << uint(attempt)
	if delay > rateLimitMaxBackoff {
		delay = rateLimitMaxBackoff
	}
	return delay + time.Duration(rand.Int63n(int64(delay/2)+1))
}

// errorLogBody truncates error bodies for logs.
func errorLogBody(e *RequestError) string {
	body := strings.TrimSpace(e.Body)
	if hint := e.rateLimitHint(); hint != "" {
		body = "[" + hint + "] " + body
	}
	if len(body) > 2048 {
		return body[:2048] + "..."
	}
	return body
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
			Body:       compactErrorBody(sdkErr),
		}
		if sdkErr.Response != nil {
			applyRateLimitHeaders(reqErr, sdkErr.Response.Header)
		}
		return reqErr
	}

	return nil
}

// compactErrorBody prefers the response's JSON error body over a full
// header+body dump so run errors and traces stay readable.
func compactErrorBody(sdkErr *sdk.Error) string {
	if raw := strings.TrimSpace(sdkErr.RawJSON()); raw != "" && raw != "null" {
		return raw
	}
	return string(sdkErr.DumpResponse(true))
}

// applyRateLimitHeaders populates retry guidance from response headers:
//   - Retry-After (delta-seconds or HTTP date)
//   - X-Should-Retry ("true"/"false"), Anthropic's explicit retry hint
//   - anthropic-ratelimit-unified-status/-reset: subscription (OAuth) usage
//     window state; reset is unix epoch seconds
//   - anthropic-ratelimit-requests-reset / -tokens-reset: API-key bucket
//     resets in RFC 3339
func applyRateLimitHeaders(e *RequestError, h http.Header) {
	e.retryAfter = parseRetryAfterSeconds(h)
	e.RateLimitReset = strings.TrimSpace(h.Get("anthropic-ratelimit-requests-reset"))
	e.UnifiedStatus = strings.TrimSpace(h.Get("anthropic-ratelimit-unified-status"))
	if raw := strings.TrimSpace(h.Get("X-Should-Retry")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			e.shouldRetry = &parsed
		}
	}
	e.resetAt = earliestRateLimitReset(h)
}

// earliestRateLimitReset returns the soonest future reset instant advertised
// by any rate-limit reset header, or the zero time when none parse.
func earliestRateLimitReset(h http.Header) time.Time {
	var earliest time.Time
	consider := func(t time.Time) {
		if t.IsZero() || !t.After(time.Now()) {
			return
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	// Unified (subscription/OAuth) reset: unix epoch seconds.
	if raw := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-reset")); raw != "" {
		if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
			consider(time.Unix(secs, 0))
		} else if t, err := time.Parse(time.RFC3339, raw); err == nil {
			consider(t)
		}
	}
	// API-key bucket resets: RFC 3339.
	for _, key := range []string{
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-reset",
		"anthropic-ratelimit-tokens-reset",
	} {
		raw := strings.TrimSpace(h.Get(key))
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			consider(t)
		}
	}
	return earliest
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
				InputTokens:              e.Usage.InputTokens,
				OutputTokens:             e.Usage.OutputTokens,
				CacheReadInputTokens:     e.Usage.CacheReadInputTokens,
				CacheCreationInputTokens: e.Usage.CacheCreationInputTokens,
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
		case "compaction":
			b := block.AsCompaction()
			compaction := NewCompactionBlock("", b.EncryptedContent, "")
			compaction.Content = b.Content
			resp.Content = append(resp.Content, compaction)
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
	case "compaction":
		b := e.ContentBlock.AsCompaction()
		return &ContentBlock{Type: "compaction", Content: b.Content, EncryptedContent: b.EncryptedContent}
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
	case "compaction_delta":
		d := e.Delta.AsCompactionDelta()
		return &DeltaBlock{Type: "compaction_delta", Content: d.Content, EncryptedContent: d.EncryptedContent}
	default:
		return &DeltaBlock{Type: string(e.Delta.Type)}
	}
}
