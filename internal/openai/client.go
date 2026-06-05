package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/internal/anthropic"
	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const (
	defaultBaseURL       = "https://api.openai.com/v1"
	defaultMaxConcurrent = 2
	maxRetries           = 3
	maxRetryAfterSeconds = 5 * 60
	apiModeResponses     = "responses"
	apiModeChat          = "chat-completions"
	// maxProviderResponseBytes caps the response body size we read from the
	// provider. A misbehaving or hostile endpoint cannot exhaust memory by
	// streaming an unbounded body in response to a single API call.
	maxProviderResponseBytes = 16 << 20 // 16 MiB
)

var logSecretRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|api_key|authorization)"\s*:\s*")[^"]+(")`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`),
}

// Option configures the OpenAI-compatible client.
type Option func(*clientConfig)

type clientConfig struct {
	baseURL        string
	maxConcurrent  int
	apiMode        string
	authSession    *OpenAIAuthSession
	modelFallbacks []string
}

// WithBaseURL overrides the OpenAI-compatible API base URL.
//
// Supported forms:
// - https://api.openai.com/v1
// - https://api.openai.com/v1/chat/completions (legacy; normalized to /v1)
// - https://api.openai.com/v1/responses (legacy; normalized to /v1)
func WithBaseURL(url string) Option {
	return func(c *clientConfig) { c.baseURL = strings.TrimSpace(url) }
}

// WithMaxConcurrent sets maximum in-flight API requests.
func WithMaxConcurrent(n int) Option {
	return func(c *clientConfig) { c.maxConcurrent = n }
}

// WithAPIMode forces which OpenAI endpoint family to use:
// - "responses" => /v1/responses
// - "chat-completions" => /v1/chat/completions
// - "" or unknown => "responses"
func WithAPIMode(mode string) Option {
	return func(c *clientConfig) { c.apiMode = mode }
}

// WithAuthSession sets the auth session used for request headers and refresh.
func WithAuthSession(session *OpenAIAuthSession) Option {
	return func(c *clientConfig) { c.authSession = session }
}

// WithModelFallbacks sets an ordered list of fallback model identifiers. For
// OpenRouter (and other OpenAI-compatible backends that honor it) these are sent
// as the request body's "models" array so the provider automatically retries the
// next model when one is unavailable or errors. The primary request model is
// always tried first; fallbacks follow in order. It is a no-op for the Responses
// API path. An empty list leaves the request unchanged.
func WithModelFallbacks(models []string) Option {
	return func(c *clientConfig) {
		c.modelFallbacks = append([]string(nil), models...)
	}
}

// Client implements OpenAI-compatible Responses APIs.
type Client struct {
	authSession    *OpenAIAuthSession
	baseURL        string
	sdk            sdk.Client
	completionsURL string
	apiMode        string
	httpClient     *http.Client
	sem            chan struct{}
	modelFallbacks []string
}

// CompactConversationResponse is the compacted context window returned by
// OpenAI Responses compaction, expressed as Anthropic-shaped messages so the
// agent layer can keep using provider-neutral RunItems.
type CompactConversationResponse struct {
	ID       string
	Messages []anthropic.Message
	Usage    anthropic.Usage
	Raw      *responses.CompactedResponse
}

// NewClient creates a new OpenAI-compatible client.
func NewClient(apiKey string, opts ...Option) *Client {
	return NewClientWithAuthSession(NewAPIKeyAuthSession(apiKey), opts...)
}

// NewClientWithAuthSession creates a new OpenAI-compatible client from an auth session.
func NewClientWithAuthSession(session *OpenAIAuthSession, opts ...Option) *Client {
	cfg := &clientConfig{
		baseURL:       defaultBaseURL,
		maxConcurrent: defaultMaxConcurrent,
		apiMode:       apiModeResponses,
		authSession:   session,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.authSession == nil {
		cfg.authSession = session
	}
	if cfg.authSession == nil {
		cfg.authSession = NewAPIKeyAuthSession("")
	}

	rawBaseURL := strings.TrimSpace(cfg.baseURL)
	baseURL := normalizeBaseURL(rawBaseURL)
	completionsURL := normalizeCompletionsURL(rawBaseURL, baseURL)
	maxConcurrent := cfg.maxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	httpClient := newAuthHTTPClient(cfg.authSession, 10*time.Minute)

	sdkClient := sdk.NewClient(
		option.WithAPIKey(cfg.authSession.sdkAPIKey()),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0), // retries are managed by doWithRetry for provider-agnostic behavior
	)

	return &Client{
		authSession:    cfg.authSession,
		baseURL:        baseURL,
		sdk:            sdkClient,
		completionsURL: completionsURL,
		apiMode:        normalizeAPIMode(cfg.apiMode),
		httpClient:     httpClient,
		sem:            make(chan struct{}, maxConcurrent),
		modelFallbacks: cfg.modelFallbacks,
	}
}

// RequestError is an OpenAI-compatible HTTP request error.
type RequestError struct {
	StatusCode int
	Body       string
	API        string
	Endpoint   string
	retryAfter int
}

func (e *RequestError) Error() string {
	api := strings.TrimSpace(e.API)
	endpoint := strings.TrimSpace(e.Endpoint)
	if api == "" && endpoint == "" {
		return fmt.Sprintf("OpenAI API request failed with status %d: %s", e.StatusCode, e.Body)
	}
	if api == "" {
		return fmt.Sprintf("OpenAI API request failed with status %d at %s: %s", e.StatusCode, endpoint, e.Body)
	}
	if endpoint == "" {
		return fmt.Sprintf("OpenAI API request failed with status %d via %s: %s", e.StatusCode, api, e.Body)
	}
	return fmt.Sprintf("OpenAI API request failed with status %d via %s at %s: %s", e.StatusCode, api, endpoint, e.Body)
}

// Retryable reports whether this error should be retried.
func (e *RequestError) Retryable() bool {
	return e.StatusCode == 429 || e.StatusCode >= 500
}

// RetryAfterMS returns Retry-After in milliseconds.
func (e *RequestError) RetryAfterMS() int {
	return capRetryAfterSeconds(e.retryAfter) * 1000
}

// ResponseFailedError preserves terminal Responses status details instead of
// collapsing them into a generic empty-output error.
type ResponseFailedError struct {
	ID               string
	Status           string
	Code             string
	Message          string
	IncompleteReason string
}

func (e *ResponseFailedError) Error() string {
	if e == nil {
		return "responses api failed"
	}
	var meta []string
	if e.ID != "" {
		meta = append(meta, fmt.Sprintf("id=%q", e.ID))
	}
	if e.Status != "" {
		meta = append(meta, fmt.Sprintf("status=%q", e.Status))
	}
	if e.Code != "" {
		meta = append(meta, fmt.Sprintf("code=%q", e.Code))
	}
	if e.IncompleteReason != "" {
		meta = append(meta, fmt.Sprintf("reason=%q", e.IncompleteReason))
	}
	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		detail = "no output content"
	}
	if len(meta) == 0 {
		return "responses api failed: " + detail
	}
	return fmt.Sprintf("responses api failed (%s): %s", strings.Join(meta, " "), detail)
}

func (e *ResponseFailedError) Retryable() bool {
	if e == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(e.Code)) {
	case "server_error", "rate_limit_exceeded", "vector_store_timeout":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(e.Status)) {
	case "failed":
		return e.Code == ""
	case "incomplete":
		return e.IncompleteReason == ""
	default:
		return false
	}
}

func (e *ResponseFailedError) NonRetryableReason() string {
	if e == nil || e.Retryable() {
		return ""
	}
	return firstNonEmpty(e.Code, e.IncompleteReason, e.Status)
}

func responseFailedError(resp *responses.Response) error {
	if resp == nil {
		return fmt.Errorf("responses api returned nil response")
	}
	return &ResponseFailedError{
		ID:               resp.ID,
		Status:           string(resp.Status),
		Code:             string(resp.Error.Code),
		Message:          resp.Error.Message,
		IncompleteReason: resp.IncompleteDetails.Reason,
	}
}

func isFailedResponseStatus(status responses.ResponseStatus) bool {
	switch strings.ToLower(strings.TrimSpace(string(status))) {
	case "failed", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func isIncompleteResponseStatus(status responses.ResponseStatus) bool {
	return strings.EqualFold(strings.TrimSpace(string(status)), "incomplete")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// StreamReader replays synthetic Anthropic-style events derived from Responses output.
type StreamReader struct {
	events []anthropic.StreamEvent
	idx    int
}

// Next returns the next synthetic stream event.
func (r *StreamReader) Next() (anthropic.StreamEvent, error) {
	if r.idx >= len(r.events) {
		return anthropic.StreamEvent{}, io.EOF
	}
	ev := r.events[r.idx]
	r.idx++
	return ev, nil
}

// Close is a no-op for buffered stream readers.
func (r *StreamReader) Close() error { return nil }

// ResponsesStreamReader wraps the SDK's real SSE stream and translates
// OpenAI Responses streaming events into Anthropic-style StreamEvents.
type ResponsesStreamReader struct {
	stream *ssestream.Stream[responses.ResponseStreamEventUnion]

	// Buffered events produced from a single SSE event that yielded
	// multiple Anthropic events (e.g. output_item.added → content_block_start).
	buf []anthropic.StreamEvent

	// Track output items for content_block indexing.
	messageStartSent bool
	blockIndex       int
	outputPhases     map[int]string
	deltaEmitted     map[int]bool
	err              error
}

// Next returns the next translated Anthropic-style stream event.
func (r *ResponsesStreamReader) Next() (anthropic.StreamEvent, error) {
	for {
		// Drain buffered events first.
		if len(r.buf) > 0 {
			ev := r.buf[0]
			r.buf = r.buf[1:]
			return ev, nil
		}

		// Read next SSE event from the underlying stream.
		if !r.stream.Next() {
			if err := r.stream.Err(); err != nil {
				return anthropic.StreamEvent{}, err
			}
			return anthropic.StreamEvent{}, io.EOF
		}

		evt := r.stream.Current()
		r.translateEvent(evt)
		if r.err != nil {
			return anthropic.StreamEvent{}, r.err
		}
	}
}

// Close closes the underlying SSE stream.
func (r *ResponsesStreamReader) Close() error {
	if r.stream != nil {
		return r.stream.Close()
	}
	return nil
}

func (r *ResponsesStreamReader) emit(ev anthropic.StreamEvent) {
	r.buf = append(r.buf, ev)
}

// markDelta records that an incremental delta was emitted for the given block
// index, so the terminal ".done" handler can avoid re-emitting the final
// payload (which would double-count content for standard OpenAI streams).
func (r *ResponsesStreamReader) markDelta(idx int) {
	if r.deltaEmitted == nil {
		r.deltaEmitted = make(map[int]bool)
	}
	r.deltaEmitted[idx] = true
}

func (r *ResponsesStreamReader) translateEvent(evt responses.ResponseStreamEventUnion) {
	switch evt.Type {
	case "response.created", "response.in_progress":
		if !r.messageStartSent {
			r.messageStartSent = true
			r.emit(anthropic.StreamEvent{
				Type: anthropic.EventMessageStart,
				Message: &anthropic.CreateMessageResponse{
					ID:    evt.Response.ID,
					Model: string(evt.Response.Model),
					Role:  anthropic.RoleAssistant,
					Usage: anthropic.Usage{
						InputTokens: evt.Response.Usage.InputTokens,
					},
				},
			})
		}

	case "response.output_item.added":
		// New output item (message, function_call, reasoning, etc.).
		idx := int(evt.OutputIndex)
		switch evt.Item.Type {
		case "message":
			// Message items contain content parts; we handle them at content_part level.
			if evt.Item.Phase != "" {
				if r.outputPhases == nil {
					r.outputPhases = make(map[int]string)
				}
				r.outputPhases[idx] = string(evt.Item.Phase)
			}
		case "function_call":
			r.blockIndex = idx
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockStart,
				Index: idx,
				ContentBlock: &anthropic.ContentBlock{
					Type: "tool_use",
					ID:   evt.Item.CallID,
					Name: evt.Item.Name,
				},
			})
		case "reasoning":
			r.blockIndex = idx
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockStart,
				Index: idx,
				ContentBlock: &anthropic.ContentBlock{
					Type:             "thinking",
					ID:               evt.Item.ID,
					EncryptedContent: evt.Item.EncryptedContent,
				},
			})
		case "compaction":
			r.blockIndex = idx
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockStart,
				Index: idx,
				ContentBlock: &anthropic.ContentBlock{
					Type:             "compaction",
					ID:               evt.Item.ID,
					EncryptedContent: evt.Item.EncryptedContent,
					CreatedBy:        evt.Item.CreatedBy,
				},
			})
		}

	case "response.content_part.added":
		// Text content part within a message item.
		if evt.Part.Type == "output_text" {
			r.blockIndex = int(evt.OutputIndex)
			phase := r.outputPhases[r.blockIndex]
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockStart,
				Index: r.blockIndex,
				ContentBlock: &anthropic.ContentBlock{
					Type:  "text",
					Phase: phase,
				},
			})
		}

	case "response.output_text.delta":
		r.markDelta(int(evt.OutputIndex))
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockDelta,
			Index: int(evt.OutputIndex),
			Delta: &anthropic.DeltaBlock{
				Type: "text_delta",
				Text: evt.Delta,
			},
		})

	case "response.function_call_arguments.delta":
		r.markDelta(int(evt.OutputIndex))
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockDelta,
			Index: int(evt.OutputIndex),
			Delta: &anthropic.DeltaBlock{
				Type:        "input_json_delta",
				PartialJSON: evt.Delta,
			},
		})

	case "response.reasoning_text.delta":
		r.markDelta(int(evt.OutputIndex))
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockDelta,
			Index: int(evt.OutputIndex),
			Delta: &anthropic.DeltaBlock{
				Type:     "thinking_delta",
				Text:     evt.Delta,
				Thinking: evt.Delta,
			},
		})

	case "response.output_text.done",
		"response.function_call_arguments.done",
		"response.reasoning_text.done":
		// Some OpenAI-compatible backends deliver the full payload only on the
		// terminal ".done" event without incremental deltas. When no delta was
		// emitted for this block, fall back to the final payload so the
		// reassembled response is not empty. Then emit block_stop. The
		// container events (response.content_part.done, response.output_item.done)
		// are ignored to avoid duplicate stops for the same block.
		idx := int(evt.OutputIndex)
		if !r.deltaEmitted[idx] {
			switch evt.Type {
			case "response.output_text.done":
				if evt.Text != "" {
					r.emit(anthropic.StreamEvent{
						Type:  anthropic.EventContentBlockDelta,
						Index: idx,
						Delta: &anthropic.DeltaBlock{Type: "text_delta", Text: evt.Text},
					})
				}
			case "response.function_call_arguments.done":
				if evt.Arguments != "" {
					r.emit(anthropic.StreamEvent{
						Type:  anthropic.EventContentBlockDelta,
						Index: idx,
						Delta: &anthropic.DeltaBlock{Type: "input_json_delta", PartialJSON: evt.Arguments},
					})
				}
			case "response.reasoning_text.done":
				if evt.Text != "" {
					r.emit(anthropic.StreamEvent{
						Type:  anthropic.EventContentBlockDelta,
						Index: idx,
						Delta: &anthropic.DeltaBlock{Type: "thinking_delta", Text: evt.Text, Thinking: evt.Text},
					})
				}
			}
		}
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockStop,
			Index: idx,
		})

	case "response.output_item.done":
		if (evt.Item.Type == "reasoning" || evt.Item.Type == "compaction") && strings.TrimSpace(evt.Item.EncryptedContent) != "" {
			deltaType := "reasoning_encrypted_content"
			if evt.Item.Type == "compaction" {
				deltaType = "compaction_encrypted_content"
			}
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockDelta,
				Index: int(evt.OutputIndex),
				Delta: &anthropic.DeltaBlock{
					Type:             deltaType,
					EncryptedContent: evt.Item.EncryptedContent,
				},
			})
		}

	case "response.completed":
		// Determine stop reason from the completed response.
		stopReason := anthropic.StopReasonEndTurn
		for _, item := range evt.Response.Output {
			if item.Type == "function_call" {
				stopReason = anthropic.StopReasonToolUse
				break
			}
		}
		if strings.EqualFold(evt.Response.IncompleteDetails.Reason, "max_output_tokens") {
			stopReason = anthropic.StopReasonMaxTokens
		}

		r.emit(anthropic.StreamEvent{
			Type: anthropic.EventMessageDelta,
			Delta: &anthropic.DeltaBlock{
				Type:       "message_delta",
				StopReason: string(stopReason),
			},
			Usage: &anthropic.Usage{
				InputTokens:          evt.Response.Usage.InputTokens,
				OutputTokens:         evt.Response.Usage.OutputTokens,
				CacheReadInputTokens: evt.Response.Usage.InputTokensDetails.CachedTokens,
			},
		})
		r.emit(anthropic.StreamEvent{Type: anthropic.EventMessageStop})
	case "response.failed":
		r.err = responseFailedError(&evt.Response)
	}
}

func (c *Client) CreateMessage(ctx context.Context, req anthropic.CreateMessageRequest) (*anthropic.CreateMessageResponse, error) {
	responseParams, err := toResponseParams(req)
	if err != nil {
		return nil, err
	}

	chatReq, err := toChatRequest(req)
	if err != nil {
		return nil, err
	}
	chatReq.Models = withModelFallbacks(chatReq.Model, c.modelFallbacks)

	var out *anthropic.CreateMessageResponse
	useResponsesFirst := c.shouldUseResponsesFirst(req.Model)
	log.Printf("[openai] request model=%q order=%s api_mode=%s base_url=%s chat_url=%s responses_url=%s", req.Model, requestOrderLabel(useResponsesFirst), c.apiMode, c.baseURL, c.completionsURL, responsesURLForBase(c.baseURL))
	err = c.doWithRetry(ctx, func(ctx context.Context) error {
		if useResponsesFirst {
			out, err = c.createViaResponses(ctx, responseParams)
			return err
		}

		out, err = c.createViaChatCompletions(ctx, chatReq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AnalyzeImage sends a single image plus prompt through the OpenAI Responses
// image-input path and returns the assembled assistant message.
func (c *Client) AnalyzeImage(ctx context.Context, model, imageURL, prompt, detail string) (*anthropic.CreateMessageResponse, error) {
	if c == nil {
		return nil, errors.New("openai client is nil")
	}
	params, err := imageAnalysisResponseParams(model, imageURL, prompt, detail)
	if err != nil {
		return nil, err
	}

	var out *anthropic.CreateMessageResponse
	err = c.doWithRetry(ctx, func(ctx context.Context) error {
		var callErr error
		out, callErr = c.createViaResponses(ctx, params)
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) shouldUseResponsesFirst(model string) bool {
	_ = model
	return c.apiMode != apiModeChat
}

func (c *Client) SupportsResponseCompaction() bool {
	if c == nil || c.apiMode != apiModeResponses {
		return false
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Host))
	return host == "api.openai.com" || isChatGPTBackendHost(host)
}

// messageStream is the streaming interface that both StreamReader and
// ResponsesStreamReader satisfy, matching agent.MessageStream.
type messageStream interface {
	Next() (anthropic.StreamEvent, error)
	Close() error
}

func (c *Client) CreateMessageStream(ctx context.Context, req anthropic.CreateMessageRequest) (messageStream, error) {
	if c.shouldUseResponsesFirst(req.Model) {
		if c.shouldUseCodexBackendResponses() {
			resp, err := c.CreateMessage(ctx, req)
			if err != nil {
				return nil, err
			}
			return &StreamReader{events: responseToEvents(resp)}, nil
		}

		responseParams, err := toResponseParams(req)
		if err != nil {
			return nil, err
		}

		// Acquire semaphore for the streaming call setup.
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		stream := c.sdk.Responses.NewStreaming(ctx, responseParams)
		<-c.sem

		return &ResponsesStreamReader{stream: stream}, nil
	}

	// Chat Completions fallback: use buffered approach.
	resp, err := c.CreateMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	return &StreamReader{events: responseToEvents(resp)}, nil
}

func (c *Client) createViaResponses(ctx context.Context, params responses.ResponseNewParams) (*anthropic.CreateMessageResponse, error) {
	log.Printf("[openai] using api=responses url=%s", responsesURLForBase(c.baseURL))

	if c.shouldUseCodexBackendResponses() {
		resp, err := c.sdk.Responses.New(ctx, params)
		if err != nil {
			return nil, withRequestContext(mapSDKError(err), "responses", responsesURLForBase(c.baseURL))
		}
		return toAnthropicResponseFromResponses(resp)
	}

	// Standard Responses uses streaming so callers get the same event path as
	// the streamed API. Codex OAuth responses are collected above because that
	// backend requires SSE even for non-streaming SDK calls.
	stream := c.sdk.Responses.NewStreaming(ctx, params)
	reader := &ResponsesStreamReader{stream: stream}
	defer reader.Close()

	return drainStreamToResponse(reader)
}

func (c *Client) CompactConversation(ctx context.Context, req anthropic.CreateMessageRequest) (*CompactConversationResponse, error) {
	if !c.SupportsResponseCompaction() {
		return nil, fmt.Errorf("responses compaction unsupported for api_mode=%s base_url=%s", c.apiMode, c.baseURL)
	}
	useCodexCompact := c.shouldUseCodexBackendCompact()
	params, err := toCompactParams(req, useCodexCompact)
	if err != nil {
		return nil, err
	}

	var compacted *responses.CompactedResponse
	log.Printf("[openai] using api=responses_compact url=%s", responsesCompactURLForBase(c.baseURL))
	err = c.doWithRetry(ctx, func(ctx context.Context) error {
		var callErr error
		if useCodexCompact {
			compacted, callErr = c.compactViaCodexBackend(ctx, params)
		} else {
			compacted, callErr = c.sdk.Responses.Compact(ctx, params)
		}
		if callErr != nil {
			return withRequestContext(mapSDKError(callErr), "responses_compact", responsesCompactURLForBase(c.baseURL))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return compactedResponseToConversation(compacted), nil
}

func (c *Client) shouldUseCodexBackendCompact() bool {
	return c != nil && c.authSession != nil && c.authSession.IsOAuth() && IsChatGPTBackendBaseURL(c.baseURL)
}

func (c *Client) shouldUseCodexBackendResponses() bool {
	return c != nil && c.authSession != nil && c.authSession.IsOAuth() && IsChatGPTBackendBaseURL(c.baseURL)
}

func (c *Client) compactViaCodexBackend(ctx context.Context, params responses.ResponseCompactParams) (*responses.CompactedResponse, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal responses compact request: %w", err)
	}
	endpoint := responsesCompactURLForBase(c.baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build responses compact request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read responses compact response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseHTTPError(resp, raw, "responses_compact", endpoint)
	}

	var compacted responses.CompactedResponse
	if err := json.Unmarshal(raw, &compacted); err != nil {
		return nil, fmt.Errorf("unmarshal responses compact response: %w", err)
	}
	return &compacted, nil
}

func (c *Client) createViaChatCompletions(ctx context.Context, body chatCompletionRequest) (*anthropic.CreateMessageResponse, error) {
	raw, err := c.doChatRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	var resp chatCompletionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal chat completion response: %w", err)
	}
	if resp.Error != nil {
		return nil, &RequestError{StatusCode: 400, Body: resp.Error.Message}
	}

	return toAnthropicResponseFromChat(resp)
}

func normalizeBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultBaseURL
	}

	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return defaultBaseURL
	}

	path := strings.TrimSuffix(strings.TrimSpace(u.Path), "/")
	path = strings.TrimSuffix(path, "/chat/completions")
	path = strings.TrimSuffix(path, "/responses")
	if path == "" {
		path = "/v1"
	}
	// Only force /v1 suffix for standard OpenAI-compatible API hosts.
	// The ChatGPT backend API (chatgpt.com) uses a non-versioned path.
	if !isChatGPTBackendHost(u.Host) && !strings.Contains(path, "/v1") {
		path = strings.TrimSuffix(path, "/") + "/v1"
	}
	u.Path = path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimSuffix(u.String(), "/")
}

func normalizeCompletionsURL(rawBaseURL, normalizedBaseURL string) string {
	trimmed := strings.TrimSpace(rawBaseURL)
	if isChatCompletionsURL(trimmed) {
		u, err := url.Parse(trimmed)
		if err == nil && u.Scheme != "" && u.Host != "" {
			u.RawPath = ""
			u.RawQuery = ""
			u.Fragment = ""
			return strings.TrimSuffix(u.String(), "/")
		}
	}
	return strings.TrimSuffix(normalizedBaseURL, "/") + "/chat/completions"
}

func isChatCompletionsURL(raw string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasSuffix(trimmed, "/chat/completions")
}

func normalizeAPIMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case apiModeResponses:
		return apiModeResponses
	case apiModeChat:
		return apiModeChat
	default:
		return apiModeResponses
	}
}

func toResponseParams(req anthropic.CreateMessageRequest) (responses.ResponseNewParams, error) {
	items, err := toResponseInputItems(req.Messages)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}

	params := responses.ResponseNewParams{
		Model:           req.Model,
		MaxOutputTokens: sdk.Int(int64(req.MaxTokens)),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: items,
		},
	}

	instructions := systemPromptText(req.System)
	// Always send instructions, even when empty. The ChatGPT Codex backend
	// may behave differently when the field is absent vs. present-but-empty.
	// The openai-oauth reference implementation always sets this field.
	params.Instructions = sdk.String(instructions)

	if len(req.Tools) > 0 {
		tools, err := responseToolsFromAnthropic(req.Tools)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Tools = tools
		params.ParallelToolCalls = sdk.Bool(true)
	}

	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		params.Reasoning = sharedReasoningFromBudget(req.Thinking.BudgetTokens)
		// Include encrypted reasoning tokens for multi-turn with store=false.
		params.Include = append(params.Include, responses.ResponseIncludable("reasoning.encrypted_content"))
	}
	if textConfig, ok, err := responseTextConfig(req); err != nil {
		return responses.ResponseNewParams{}, err
	} else if ok {
		params.Text = textConfig
	}
	if req.CompactionThreshold > 0 {
		params.ContextManagement = []responses.ResponseNewParamsContextManagement{
			{
				Type:             "compaction",
				CompactThreshold: sdk.Int(int64(req.CompactionThreshold)),
			},
		}
	}

	// Auto-truncate input to fit context window instead of returning 400.
	params.Truncation = responses.ResponseNewParamsTruncation("auto")

	// Extended prompt cache retention for better cache hit rates.
	params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetention("24h")

	return params, nil
}

func imageAnalysisResponseParams(model, imageURL, prompt, detail string) (responses.ResponseNewParams, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultChatModel
	}
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return responses.ResponseNewParams{}, fmt.Errorf("image URL is required")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return responses.ResponseNewParams{}, fmt.Errorf("prompt is required")
	}

	imageContent := responses.ResponseInputContentParamOfInputImage(normalizeImageAnalysisDetail(detail))
	imageContent.OfInputImage.ImageURL = param.NewOpt(imageURL)
	content := responses.ResponseInputMessageContentListParam{
		responses.ResponseInputContentParamOfInputText(prompt),
		imageContent,
	}

	params := responses.ResponseNewParams{
		Model:           model,
		MaxOutputTokens: sdk.Int(4096),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam{
				responses.ResponseInputItemParamOfMessage(content, responses.EasyInputMessageRoleUser),
			},
		},
	}
	params.Instructions = sdk.String("Analyze the image and answer the user's prompt. Be concise and specific.")
	params.Truncation = responses.ResponseNewParamsTruncation("auto")
	params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetention("24h")
	return params, nil
}

func normalizeImageAnalysisDetail(detail string) responses.ResponseInputImageDetail {
	switch strings.ToLower(strings.TrimSpace(detail)) {
	case "low":
		return responses.ResponseInputImageDetailLow
	case "original":
		return responses.ResponseInputImageDetailOriginal
	case "auto":
		return responses.ResponseInputImageDetailAuto
	default:
		return responses.ResponseInputImageDetailHigh
	}
}

func toCompactParams(req anthropic.CreateMessageRequest, includeCodexExtras bool) (responses.ResponseCompactParams, error) {
	items, err := toResponseInputItems(req.Messages)
	if err != nil {
		return responses.ResponseCompactParams{}, err
	}
	params := responses.ResponseCompactParams{
		Model: responses.ResponseCompactParamsModel(req.Model),
		Input: responses.ResponseCompactParamsInputUnion{
			OfResponseInputItemArray: items,
		},
	}
	instructions := systemPromptText(req.System)
	if instructions != "" || !includeCodexExtras {
		params.Instructions = sdk.String(instructions)
	}
	if includeCodexExtras {
		extras := map[string]any{
			"tools":               []responses.ToolUnionParam{},
			"parallel_tool_calls": false,
		}
		if len(req.Tools) > 0 {
			tools, err := responseToolsFromAnthropic(req.Tools)
			if err != nil {
				return responses.ResponseCompactParams{}, err
			}
			extras["tools"] = tools
			extras["parallel_tool_calls"] = true
		}
		if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
			extras["reasoning"] = sharedReasoningFromBudget(req.Thinking.BudgetTokens)
		}
		if verbosity := normalizeTextVerbosity(req.TextVerbosity); verbosity != "" {
			extras["text"] = responses.ResponseTextConfigParam{
				Verbosity: responses.ResponseTextConfigVerbosity(verbosity),
			}
		}
		params.SetExtraFields(extras)
	}
	return params, nil
}

func responseToolsFromAnthropic(tools []anthropic.ToolDefinition) ([]responses.ToolUnionParam, error) {
	out := make([]responses.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema, err := normalizedToolSchema(t.Name, t.InputSchema)
		if err != nil {
			return nil, err
		}
		out = append(out, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        t.Name,
				Description: sdk.String(t.Description),
				Parameters:  schema,
				Strict:      sdk.Bool(false),
			},
		})
	}
	return out, nil
}

func normalizedToolSchema(name string, raw json.RawMessage) (map[string]any, error) {
	schema := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, fmt.Errorf("unmarshal tool schema for %s: %w", name, err)
		}
	}
	if typ, _ := schema["type"].(string); typ == "" || typ == "object" {
		schema["type"] = "object"
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
	}
	return schema, nil
}

func responseTextConfig(req anthropic.CreateMessageRequest) (responses.ResponseTextConfigParam, bool, error) {
	var cfg responses.ResponseTextConfigParam
	ok := false
	if verbosity := normalizeTextVerbosity(req.TextVerbosity); verbosity != "" {
		cfg.Verbosity = responses.ResponseTextConfigVerbosity(verbosity)
		ok = true
	}
	if req.OutputSchema != nil {
		format, err := responseFormatFromSchema(req.OutputSchema)
		if err != nil {
			return responses.ResponseTextConfigParam{}, false, err
		}
		cfg.Format = format
		ok = true
	}
	return cfg, ok, nil
}

func responseFormatFromSchema(schema *anthropic.OutputSchema) (responses.ResponseFormatTextConfigUnionParam, error) {
	schemaMap, err := outputSchemaMap(schema)
	if err != nil {
		return responses.ResponseFormatTextConfigUnionParam{}, err
	}
	return responses.ResponseFormatTextConfigUnionParam{
		OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name:   schema.Name,
			Schema: schemaMap,
			Strict: param.NewOpt(schema.Strict),
		},
	}, nil
}

func chatResponseFormat(schema *anthropic.OutputSchema) (map[string]any, error) {
	schemaMap, err := outputSchemaMap(schema)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   schema.Name,
			"schema": schemaMap,
			"strict": schema.Strict,
		},
	}, nil
}

func outputSchemaMap(schema *anthropic.OutputSchema) (map[string]any, error) {
	if schema == nil {
		return nil, nil
	}
	var schemaMap map[string]any
	if len(schema.Schema) > 0 {
		if err := json.Unmarshal(schema.Schema, &schemaMap); err != nil {
			return nil, fmt.Errorf("unmarshal output schema %s: %w", schema.Name, err)
		}
	}
	if schemaMap == nil {
		schemaMap = map[string]any{"type": "object"}
	}
	return schemaMap, nil
}

func sharedReasoningFromBudget(budget int) shared.ReasoningParam {
	// Rough mapping from internal thinking budgets to OpenAI effort tiers.
	// Thresholds are tuned to keep Anthropic budgets comfortably below the
	// default max token limit while still producing distinct OpenAI effort levels.
	effort := "minimal"
	switch {
	case budget >= 12288:
		effort = "xhigh"
	case budget >= 8192:
		effort = "high"
	case budget >= 4096:
		effort = "medium"
	case budget >= 2048:
		effort = "low"
	}
	return shared.ReasoningParam{Effort: shared.ReasoningEffort(effort)}
}

func systemPromptText(system []anthropic.SystemBlock) string {
	if len(system) == 0 {
		return ""
	}
	parts := make([]string, 0, len(system))
	for _, s := range system {
		if text := strings.TrimSpace(s.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func toResponseInputItems(messages []anthropic.Message) (responses.ResponseInputParam, error) {
	items := make(responses.ResponseInputParam, 0, len(messages))
	seenToolCalls := map[string]struct{}{}

	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(string(m.Role)))
		for _, block := range m.Content {
			switch block.Type {
			case "text":
				text := strings.TrimSpace(block.Text)
				if text == "" {
					continue
				}
				msg := &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRole(role),
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: sdk.String(text),
					},
				}
				if role == "assistant" {
					msg.Phase = resolveAssistantPhase(block, m.Content)
				}
				items = append(items, responses.ResponseInputItemUnionParam{OfMessage: msg})

			case "tool_use":
				if role != "assistant" {
					continue
				}
				callID := strings.TrimSpace(block.ID)
				if callID == "" {
					callID = fmt.Sprintf("call_%d", len(items))
				}
				arguments := strings.TrimSpace(string(normalizeArguments(string(block.Input))))
				items = append(items, responses.ResponseInputItemUnionParam{
					OfFunctionCall: &responses.ResponseFunctionToolCallParam{
						CallID:    callID,
						Name:      block.Name,
						Arguments: arguments,
					},
				})
				seenToolCalls[callID] = struct{}{}

			case "tool_result":
				callID := strings.TrimSpace(block.ToolUseID)
				if callID == "" {
					continue
				}
				if _, ok := seenToolCalls[callID]; !ok {
					continue
				}
				output := block.Content
				if strings.TrimSpace(output) == "" {
					output = "(no output)"
				}
				items = append(items, responses.ResponseInputItemUnionParam{
					OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
						CallID: callID,
						Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
							OfString: sdk.String(output),
						},
					},
				})

			case "image":
				if block.Source == nil || strings.TrimSpace(block.Source.Data) == "" {
					continue
				}
				mediaType := strings.TrimSpace(block.Source.MediaType)
				if mediaType == "" {
					mediaType = "image/png"
				}
				imageContent := responses.ResponseInputContentParamOfInputImage(responses.ResponseInputImageDetailAuto)
				imageContent.OfInputImage.ImageURL = param.NewOpt(fmt.Sprintf("data:%s;base64,%s", mediaType, block.Source.Data))
				content := responses.ResponseInputMessageContentListParam{imageContent}
				items = append(items, responses.ResponseInputItemParamOfMessage(content, responses.EasyInputMessageRole(role)))

			case "thinking", "redacted_thinking":
				if role != "assistant" || strings.TrimSpace(block.EncryptedContent) == "" {
					continue
				}
				id := strings.TrimSpace(block.ID)
				if id == "" {
					id = fmt.Sprintf("reasoning_%d", len(items))
				}
				item := responses.ResponseInputItemParamOfReasoning(id, nil)
				item.OfReasoning.EncryptedContent = sdk.String(block.EncryptedContent)
				item.OfReasoning.Status = responses.ResponseReasoningItemStatusCompleted
				items = append(items, item)
			case "compaction":
				encryptedContent := strings.TrimSpace(block.EncryptedContent)
				if encryptedContent == "" {
					continue
				}
				item := responses.ResponseInputItemParamOfCompaction(encryptedContent)
				if item.OfCompaction != nil && strings.TrimSpace(block.ID) != "" {
					item.OfCompaction.ID = sdk.String(block.ID)
				}
				items = append(items, item)
			}
		}
	}

	return items, nil
}

func resolveAssistantPhase(block anthropic.ContentBlock, blocks []anthropic.ContentBlock) responses.EasyInputMessagePhase {
	switch strings.ToLower(strings.TrimSpace(block.Phase)) {
	case string(responses.EasyInputMessagePhaseCommentary):
		return responses.EasyInputMessagePhaseCommentary
	case string(responses.EasyInputMessagePhaseFinalAnswer):
		return responses.EasyInputMessagePhaseFinalAnswer
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return responses.EasyInputMessagePhaseCommentary
		}
	}
	return responses.EasyInputMessagePhaseFinalAnswer
}

func normalizeTextVerbosity(verbosity string) string {
	switch strings.ToLower(strings.TrimSpace(verbosity)) {
	case string(responses.ResponseTextConfigVerbosityLow):
		return string(responses.ResponseTextConfigVerbosityLow)
	case string(responses.ResponseTextConfigVerbosityMedium):
		return string(responses.ResponseTextConfigVerbosityMedium)
	case string(responses.ResponseTextConfigVerbosityHigh):
		return string(responses.ResponseTextConfigVerbosityHigh)
	default:
		return ""
	}
}

func toAnthropicResponseFromResponses(resp *responses.Response) (*anthropic.CreateMessageResponse, error) {
	if resp == nil {
		return nil, fmt.Errorf("responses api returned nil response")
	}
	if isFailedResponseStatus(resp.Status) {
		return nil, responseFailedError(resp)
	}

	out := &anthropic.CreateMessageResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  anthropic.RoleAssistant,
		Model: string(resp.Model),
		Usage: anthropic.Usage{
			InputTokens:          resp.Usage.InputTokens,
			OutputTokens:         resp.Usage.OutputTokens,
			CacheReadInputTokens: resp.Usage.InputTokensDetails.CachedTokens,
		},
	}

	for idx, item := range resp.Output {
		switch item.Type {
		case "message":
			phase := string(item.Phase)
			for _, c := range item.Content {
				if (c.Type == "output_text" || c.Type == "input_text" || c.Type == "") && strings.TrimSpace(c.Text) != "" {
					block := anthropic.NewTextBlock(c.Text)
					block.Phase = phase
					out.Content = append(out.Content, block)
				}
			}
		case "reasoning":
			var thinkingText string
			for _, c := range item.Content {
				if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
					thinkingText += c.Text
				}
			}
			if thinkingText != "" || item.EncryptedContent != "" {
				out.Content = append(out.Content, anthropic.ContentBlock{
					Type:             "thinking",
					ID:               item.ID,
					Thinking:         thinkingText,
					EncryptedContent: item.EncryptedContent,
				})
			}
		case "function_call":
			callID := strings.TrimSpace(item.CallID)
			if callID == "" {
				callID = fmt.Sprintf("call_%d", idx)
			}
			out.Content = append(out.Content, anthropic.NewToolUseBlock(callID, item.Name, normalizeArguments(item.Arguments.OfString)))
		case "compaction":
			out.Content = append(out.Content, anthropic.NewCompactionBlock(item.ID, item.EncryptedContent, item.CreatedBy))
		case "web_search_call", "file_search_call", "code_interpreter_call",
			"mcp_call", "computer_call", "image_generation_call",
			"tool_search_call", "local_shell_call", "shell_call",
			"apply_patch_call", "custom_tool_call":
			callID := strings.TrimSpace(item.CallID)
			if callID == "" {
				callID = fmt.Sprintf("call_%s_%d", item.Type, idx)
			}
			name := item.Name
			if name == "" {
				name = item.Type
			}
			args := normalizeArguments(item.Arguments.OfString)
			out.Content = append(out.Content, anthropic.NewToolUseBlock(callID, name, args))
		}
	}

	hasToolCalls := false
	for _, c := range out.Content {
		if c.Type == "tool_use" {
			hasToolCalls = true
			break
		}
	}

	if hasToolCalls {
		out.StopReason = anthropic.StopReasonToolUse
	} else if strings.EqualFold(resp.IncompleteDetails.Reason, "max_output_tokens") {
		out.StopReason = anthropic.StopReasonMaxTokens
	} else {
		out.StopReason = anthropic.StopReasonEndTurn
	}

	if len(out.Content) == 0 {
		fallback := strings.TrimSpace(resp.OutputText())
		if fallback == "" {
			if isFailedResponseStatus(resp.Status) || isIncompleteResponseStatus(resp.Status) {
				return nil, responseFailedError(resp)
			}
			return nil, fmt.Errorf("responses api returned no output content (id=%q status=%q)", resp.ID, resp.Status)
		}
		out.Content = append(out.Content, anthropic.NewTextBlock(fallback))
	}

	return out, nil
}

func compactedResponseToConversation(resp *responses.CompactedResponse) *CompactConversationResponse {
	out := &CompactConversationResponse{}
	if resp == nil {
		return out
	}
	out.ID = resp.ID
	out.Usage = anthropic.Usage{
		InputTokens:          resp.Usage.InputTokens,
		OutputTokens:         resp.Usage.OutputTokens,
		CacheReadInputTokens: resp.Usage.InputTokensDetails.CachedTokens,
	}
	out.Raw = resp

	for idx, item := range resp.Output {
		switch item.Type {
		case "message":
			role := outputItemRole(item)
			var blocks []anthropic.ContentBlock
			for _, c := range item.Content {
				if strings.TrimSpace(c.Text) == "" {
					continue
				}
				block := anthropic.NewTextBlock(c.Text)
				block.Phase = string(item.Phase)
				blocks = append(blocks, block)
			}
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, anthropic.Message{Role: role, Content: blocks})
			}
		case "function_call":
			callID := strings.TrimSpace(item.CallID)
			if callID == "" {
				callID = fmt.Sprintf("call_%d", idx)
			}
			out.Messages = append(out.Messages, anthropic.Message{
				Role: anthropic.RoleAssistant,
				Content: []anthropic.ContentBlock{
					anthropic.NewToolUseBlock(callID, item.Name, normalizeArguments(item.Arguments.OfString)),
				},
			})
		case "function_call_output":
			callID := strings.TrimSpace(item.CallID)
			if callID == "" {
				callID = fmt.Sprintf("call_%d", idx)
			}
			out.Messages = append(out.Messages, anthropic.Message{
				Role: anthropic.RoleUser,
				Content: []anthropic.ContentBlock{
					anthropic.NewToolResultBlock(callID, responseOutputString(item.Output), false),
				},
			})
		case "reasoning":
			if strings.TrimSpace(item.EncryptedContent) != "" {
				block := anthropic.NewThinkingBlock("")
				block.ID = item.ID
				block.EncryptedContent = item.EncryptedContent
				out.Messages = append(out.Messages, anthropic.Message{
					Role:    anthropic.RoleAssistant,
					Content: []anthropic.ContentBlock{block},
				})
			}
		case "compaction":
			out.Messages = append(out.Messages, anthropic.Message{
				Role: anthropic.RoleAssistant,
				Content: []anthropic.ContentBlock{
					anthropic.NewCompactionBlock(item.ID, item.EncryptedContent, item.CreatedBy),
				},
			})
		}
	}
	return out
}

func outputItemRole(item responses.ResponseOutputItemUnion) anthropic.Role {
	role := strings.ToLower(strings.TrimSpace(string(item.Role)))
	if role == "" && strings.TrimSpace(item.RawJSON()) != "" {
		var raw struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal([]byte(item.RawJSON()), &raw); err == nil {
			role = strings.ToLower(strings.TrimSpace(raw.Role))
		}
	}
	if role == "user" {
		return anthropic.RoleUser
	}
	return anthropic.RoleAssistant
}

func responseOutputString(output responses.ResponseOutputItemUnionOutput) string {
	if strings.TrimSpace(output.OfString) != "" {
		return output.OfString
	}
	if len(output.OfOutputContentList) > 0 {
		data, err := json.Marshal(output.OfOutputContentList)
		if err == nil {
			return string(data)
		}
	}
	return ""
}

// drainStreamToResponse reads all events from a messageStream and
// reassembles them into a CreateMessageResponse. This is used by
// createViaResponses so that the non-streaming CreateMessage path still
// goes through SSE — the Codex backend only delivers output via streaming.
func drainStreamToResponse(stream messageStream) (*anthropic.CreateMessageResponse, error) {
	resp := &anthropic.CreateMessageResponse{
		Type: "message",
		Role: anthropic.RoleAssistant,
	}

	// blocks tracks content being accumulated by index.
	type block struct {
		typ              string // "text", "tool_use", "thinking"
		id               string
		name             string
		encryptedContent string
		textBuf          strings.Builder
		inputBuf         strings.Builder
		thinkingBuf      strings.Builder
	}
	blocks := map[int]*block{}

	getBlock := func(idx int) *block {
		b, ok := blocks[idx]
		if !ok {
			b = &block{}
			blocks[idx] = b
		}
		return b
	}

	for {
		ev, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("streaming response: %w", err)
		}

		switch ev.Type {
		case anthropic.EventMessageStart:
			if ev.Message != nil {
				resp.ID = ev.Message.ID
				resp.Model = ev.Message.Model
				resp.Usage.InputTokens = ev.Message.Usage.InputTokens
			}

		case anthropic.EventContentBlockStart:
			if ev.ContentBlock != nil {
				b := getBlock(ev.Index)
				b.typ = ev.ContentBlock.Type
				b.id = ev.ContentBlock.ID
				b.name = ev.ContentBlock.Name
				b.encryptedContent = ev.ContentBlock.EncryptedContent
			}

		case anthropic.EventContentBlockDelta:
			if ev.Delta != nil {
				b := getBlock(ev.Index)
				switch ev.Delta.Type {
				case "text_delta":
					b.textBuf.WriteString(ev.Delta.Text)
				case "input_json_delta":
					b.inputBuf.WriteString(ev.Delta.PartialJSON)
				case "thinking_delta":
					if ev.Delta.Thinking != "" {
						b.thinkingBuf.WriteString(ev.Delta.Thinking)
					} else {
						b.thinkingBuf.WriteString(ev.Delta.Text)
					}
				case "reasoning_encrypted_content":
					b.encryptedContent = ev.Delta.EncryptedContent
				case "compaction_encrypted_content":
					b.encryptedContent = ev.Delta.EncryptedContent
				}
			}

		case anthropic.EventMessageDelta:
			if ev.Delta != nil {
				resp.StopReason = anthropic.StopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				if ev.Usage.InputTokens != 0 || resp.Usage.InputTokens == 0 {
					resp.Usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens != 0 || resp.Usage.OutputTokens == 0 {
					resp.Usage.OutputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.CacheReadInputTokens != 0 || resp.Usage.CacheReadInputTokens == 0 {
					resp.Usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
				}
				if ev.Usage.CacheCreationInputTokens != 0 || resp.Usage.CacheCreationInputTokens == 0 {
					resp.Usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
				}
			}
		}
	}

	// Assemble content blocks in index order.
	maxIdx := -1
	for idx := range blocks {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	for i := 0; i <= maxIdx; i++ {
		b, ok := blocks[i]
		if !ok {
			continue
		}
		switch b.typ {
		case "text":
			resp.Content = append(resp.Content, anthropic.NewTextBlock(b.textBuf.String()))
		case "tool_use":
			resp.Content = append(resp.Content, anthropic.NewToolUseBlock(b.id, b.name, json.RawMessage(b.inputBuf.String())))
		case "thinking":
			resp.Content = append(resp.Content, anthropic.ContentBlock{
				Type:             "thinking",
				ID:               b.id,
				Thinking:         b.thinkingBuf.String(),
				EncryptedContent: b.encryptedContent,
			})
		case "compaction":
			resp.Content = append(resp.Content, anthropic.NewCompactionBlock(b.id, b.encryptedContent, ""))
		}
	}

	if len(resp.Content) == 0 {
		return nil, fmt.Errorf("streaming response ended without output content (id=%q model=%q stop_reason=%q)", resp.ID, resp.Model, resp.StopReason)
	}

	return resp, nil
}

func buildChatCompletionsRequest(ctx context.Context, endpoint string, body chatCompletionRequest) (*http.Request, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completion request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (c *Client) doChatRequest(ctx context.Context, body chatCompletionRequest) ([]byte, error) {
	req, err := buildChatCompletionsRequest(ctx, c.completionsURL, body)
	if err != nil {
		return nil, err
	}
	log.Printf("[openai] using api=chat_completions url=%s", c.completionsURL)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseHTTPError(resp, raw, "chat_completions", req.URL.String())
	}
	return raw, nil
}

func mapSDKError(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		body := strings.TrimSpace(apiErr.Message)
		if body == "" {
			body = strings.TrimSpace(apiErr.Error())
		}
		// Debug: log the full error for 400s to diagnose backend rejections.
		if apiErr.StatusCode == http.StatusBadRequest {
			log.Printf("[openai] 400 response detail: message=%q body=%q", sanitizeLogBody(apiErr.Message), sanitizeLogBody(apiErr.Error()))
			if apiErr.Response != nil {
				raw, _ := io.ReadAll(io.LimitReader(apiErr.Response.Body, 2048))
				if len(raw) > 0 {
					log.Printf("[openai] 400 raw response body: %s", sanitizeLogBody(string(raw)))
				}
			}
		}
		reqErr := &RequestError{StatusCode: apiErr.StatusCode, Body: body}
		if apiErr.Response != nil {
			reqErr.retryAfter = parseRetryAfterSeconds(apiErr.Response.Header)
		}
		return reqErr
	}
	return err
}

func shouldFallbackToChatCompletions(err error) bool {
	reqErr := asRequestError(err)
	if reqErr == nil {
		return false
	}
	if reqErr.StatusCode == http.StatusNotFound || reqErr.StatusCode == http.StatusMethodNotAllowed || reqErr.StatusCode == http.StatusNotImplemented {
		return true
	}
	if reqErr.StatusCode != http.StatusBadRequest && reqErr.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	body := strings.ToLower(reqErr.Body)
	if !strings.Contains(body, "response") {
		return false
	}
	return strings.Contains(body, "unsupported") ||
		strings.Contains(body, "not support") ||
		strings.Contains(body, "unknown") ||
		strings.Contains(body, "invalid") ||
		strings.Contains(body, "not found")
}

func shouldFallbackToResponses(err error) bool {
	reqErr := asRequestError(err)
	if reqErr == nil {
		return false
	}
	if reqErr.StatusCode == http.StatusNotFound || reqErr.StatusCode == http.StatusMethodNotAllowed || reqErr.StatusCode == http.StatusNotImplemented {
		return true
	}
	if reqErr.StatusCode != http.StatusBadRequest && reqErr.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	body := strings.ToLower(reqErr.Body)
	if !strings.Contains(body, "chat") && !strings.Contains(body, "completion") {
		return false
	}
	return strings.Contains(body, "unsupported") ||
		strings.Contains(body, "not support") ||
		strings.Contains(body, "unknown") ||
		strings.Contains(body, "invalid") ||
		strings.Contains(body, "not found")
}

func parseRetryAfterSeconds(h map[string][]string) int {
	for _, key := range []string{"Retry-After-Ms", "Retry-After", "retry-after-ms", "retry-after"} {
		vals := h[key]
		if len(vals) == 0 {
			continue
		}
		raw := strings.TrimSpace(vals[0])
		if raw == "" {
			continue
		}
		if key == "Retry-After-Ms" || key == "retry-after-ms" {
			if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
				secs := ms / 1000
				if secs == 0 {
					secs = 1
				}
				return secs
			}
		}
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return secs
		}
	}
	return 0
}

func (c *Client) doWithRetry(ctx context.Context, fn func(context.Context) error) error {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			baseDelay := time.Duration(1<<uint(attempt-1)) * time.Second
			jitter := time.Duration(rand.Int63n(int64(baseDelay / 2)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(baseDelay + jitter):
			}
		}

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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		var responseErr *ResponseFailedError
		if errors.As(err, &responseErr) && !responseErr.Retryable() {
			return err
		}

		reqErr := asRequestError(err)
		if reqErr == nil {
			if attempt < maxRetries {
				continue
			}
			return err
		}

		if !reqErr.Retryable() || attempt >= maxRetries {
			return reqErr
		}
		retryAfter := capRetryAfterSeconds(reqErr.retryAfter)
		if reqErr.StatusCode == http.StatusTooManyRequests {
			log.Printf("[openai] 429 api=%s url=%s retry_after_seconds=%d attempt=%d/%d body=%s", reqErr.API, reqErr.Endpoint, retryAfter, attempt+1, maxRetries+1, sanitizeLogBody(reqErr.Body))
		}

		if retryAfter > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(retryAfter) * time.Second):
			}
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

func asRequestError(err error) *RequestError {
	var reqErr *RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}
	return nil
}

func parseHTTPError(resp *http.Response, body []byte, api, endpoint string) *RequestError {
	reqErr := &RequestError{
		StatusCode: resp.StatusCode,
		Body:       string(body),
		API:        api,
		Endpoint:   endpoint,
	}
	reqErr.retryAfter = parseRetryAfterSeconds(resp.Header)
	return reqErr
}

func withRequestContext(err error, api, endpoint string) error {
	reqErr := asRequestError(err)
	if reqErr == nil {
		return err
	}
	if strings.TrimSpace(reqErr.API) == "" {
		reqErr.API = api
	}
	if strings.TrimSpace(reqErr.Endpoint) == "" {
		reqErr.Endpoint = endpoint
	}
	return reqErr
}

func requestOrderLabel(useResponsesFirst bool) string {
	if useResponsesFirst {
		return "responses-first"
	}
	return "chat-completions-first"
}

func responsesURLForBase(baseURL string) string {
	return strings.TrimSuffix(strings.TrimSpace(baseURL), "/") + "/responses"
}

func responsesCompactURLForBase(baseURL string) string {
	return strings.TrimSuffix(strings.TrimSpace(baseURL), "/") + "/responses/compact"
}

func sanitizeLogBody(body string) string {
	trimmed := strings.Join(strings.Fields(strings.TrimSpace(body)), " ")
	for _, redactor := range logSecretRedactors {
		trimmed = redactor.ReplaceAllString(trimmed, "${1}[REDACTED]${2}")
	}
	if len(trimmed) > 500 {
		return trimmed[:500] + "...(truncated)"
	}
	return trimmed
}

type chatCompletionRequest struct {
	Model          string        `json:"model"`
	Models         []string      `json:"models,omitempty"`
	Messages       []chatMessage `json:"messages"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	Tools          []chatToolDef `json:"tools,omitempty"`
	ResponseFormat any           `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

type chatToolDef struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Arguments   string `json:"arguments,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatCompletionResponse struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chatChoice  `json:"choices"`
	Usage   *chatUsage    `json:"usage"`
	Error   *chatAPIError `json:"error,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type chatAPIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// withModelFallbacks builds the OpenRouter-style "models" array for a request.
// It returns nil when no fallbacks are configured so the request body is
// unchanged and only "model" is sent. When fallbacks exist, the primary model is
// placed first, followed by the fallbacks in order, with empty entries and
// duplicates removed so the provider tries each candidate at most once.
func withModelFallbacks(primary string, fallbacks []string) []string {
	if len(fallbacks) == 0 {
		return nil
	}
	ordered := append([]string{primary}, fallbacks...)
	seen := make(map[string]bool, len(ordered))
	out := make([]string, 0, len(ordered))
	for _, m := range ordered {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) <= 1 {
		return nil
	}
	return out
}

func toChatRequest(req anthropic.CreateMessageRequest) (chatCompletionRequest, error) {
	chatMsgs, err := toChatMessages(req)
	if err != nil {
		return chatCompletionRequest{}, err
	}

	out := chatCompletionRequest{
		Model:     req.Model,
		Messages:  chatMsgs,
		MaxTokens: req.MaxTokens,
	}

	if len(req.Tools) > 0 {
		out.Tools = make([]chatToolDef, 0, len(req.Tools))
		for _, t := range req.Tools {
			params, err := normalizedToolSchema(t.Name, t.InputSchema)
			if err != nil {
				return chatCompletionRequest{}, err
			}
			out.Tools = append(out.Tools, chatToolDef{
				Type: "function",
				Function: chatFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
	}
	if req.OutputSchema != nil {
		format, err := chatResponseFormat(req.OutputSchema)
		if err != nil {
			return chatCompletionRequest{}, err
		}
		out.ResponseFormat = format
	}

	return out, nil
}

func toChatMessages(req anthropic.CreateMessageRequest) ([]chatMessage, error) {
	var msgs []chatMessage

	if len(req.System) > 0 {
		var parts []string
		for _, s := range req.System {
			if text := strings.TrimSpace(s.Text); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			msgs = append(msgs, chatMessage{
				Role:    "system",
				Content: strings.Join(parts, "\n\n"),
			})
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case anthropic.RoleUser:
			var textParts []string
			for _, block := range m.Content {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) != "" {
						textParts = append(textParts, block.Text)
					}
				case "tool_result":
					if len(textParts) > 0 {
						msgs = append(msgs, chatMessage{
							Role:    "user",
							Content: strings.Join(textParts, "\n"),
						})
						textParts = nil
					}

					content := block.Content
					if content == "" {
						content = "(no output)"
					}
					msgs = append(msgs, chatMessage{
						Role:       "tool",
						ToolCallID: block.ToolUseID,
						Content:    content,
					})
				}
			}
			if len(textParts) > 0 {
				msgs = append(msgs, chatMessage{
					Role:    "user",
					Content: strings.Join(textParts, "\n"),
				})
			}

		case anthropic.RoleAssistant:
			var textParts []string
			var toolCalls []chatToolCall
			for _, block := range m.Content {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) != "" {
						textParts = append(textParts, block.Text)
					}
				case "tool_use":
					args := strings.TrimSpace(string(block.Input))
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, chatToolCall{
						ID:   block.ID,
						Type: "function",
						Function: chatFunction{
							Name:      block.Name,
							Arguments: args,
						},
					})
				}
			}

			msg := chatMessage{Role: "assistant"}
			if len(textParts) > 0 {
				msg.Content = strings.Join(textParts, "\n")
			}
			if len(toolCalls) > 0 {
				msg.ToolCalls = toolCalls
			}
			if msg.Content == nil && len(msg.ToolCalls) == 0 {
				msg.Content = ""
			}
			msgs = append(msgs, msg)
		}
	}

	return msgs, nil
}

func toAnthropicResponseFromChat(resp chatCompletionResponse) (*anthropic.CreateMessageResponse, error) {
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("chat completion returned no choices")
	}

	choice := resp.Choices[0]

	out := &anthropic.CreateMessageResponse{
		ID:         resp.ID,
		Type:       "message",
		Role:       anthropic.RoleAssistant,
		Model:      resp.Model,
		StopReason: mapFinishReason(choice.FinishReason),
	}
	if resp.Usage != nil {
		out.Usage = anthropic.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}

	text := extractTextContent(choice.Message.Content)
	if strings.TrimSpace(text) != "" {
		out.Content = append(out.Content, anthropic.NewTextBlock(text))
	}
	for idx, tc := range choice.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		args := normalizeArguments(tc.Function.Arguments)
		out.Content = append(out.Content, anthropic.NewToolUseBlock(id, tc.Function.Name, args))
	}

	if out.StopReason == "" {
		if len(choice.Message.ToolCalls) > 0 {
			out.StopReason = anthropic.StopReasonToolUse
		} else {
			out.StopReason = anthropic.StopReasonEndTurn
		}
	}

	return out, nil
}

func extractTextContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "text" {
				continue
			}
			if text, _ := m["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func mapFinishReason(reason string) anthropic.StopReason {
	switch reason {
	case "tool_calls":
		return anthropic.StopReasonToolUse
	case "length":
		return anthropic.StopReasonMaxTokens
	default:
		return anthropic.StopReasonEndTurn
	}
}

func responseToEvents(resp *anthropic.CreateMessageResponse) []anthropic.StreamEvent {
	events := make([]anthropic.StreamEvent, 0, len(resp.Content)*3+3)

	events = append(events, anthropic.StreamEvent{
		Type: anthropic.EventMessageStart,
		Message: &anthropic.CreateMessageResponse{
			ID:    resp.ID,
			Model: resp.Model,
			Role:  anthropic.RoleAssistant,
			Usage: anthropic.Usage{
				InputTokens: resp.Usage.InputTokens,
			},
		},
	})

	for i, block := range resp.Content {
		start := anthropic.ContentBlock{Type: block.Type}
		if block.Type == "tool_use" {
			start.ID = block.ID
			start.Name = block.Name
		}
		events = append(events, anthropic.StreamEvent{
			Type:         anthropic.EventContentBlockStart,
			Index:        i,
			ContentBlock: &start,
		})

		switch block.Type {
		case "text":
			events = append(events, anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockDelta,
				Index: i,
				Delta: &anthropic.DeltaBlock{
					Type: "text_delta",
					Text: block.Text,
				},
			})
		case "tool_use":
			events = append(events, anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockDelta,
				Index: i,
				Delta: &anthropic.DeltaBlock{
					Type:        "input_json_delta",
					PartialJSON: string(block.Input),
				},
			})
		}

		events = append(events, anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockStop,
			Index: i,
		})
	}

	events = append(events, anthropic.StreamEvent{
		Type: anthropic.EventMessageDelta,
		Delta: &anthropic.DeltaBlock{
			Type:       "message_delta",
			StopReason: string(resp.StopReason),
		},
		Usage: &anthropic.Usage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
		},
	})

	events = append(events, anthropic.StreamEvent{Type: anthropic.EventMessageStop})
	return events
}

func normalizeArguments(raw string) json.RawMessage {
	args := strings.TrimSpace(raw)
	if args == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	return json.RawMessage("{}")
}
