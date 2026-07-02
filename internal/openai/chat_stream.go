package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/gratefulagents/sdk/internal/anthropic"
)

// hostFromURL extracts the host component of a URL, returning "" on parse error.
func hostFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

// chatCompletionChunk is a single Server-Sent Event payload from a streaming
// chat-completions response.
type chatCompletionChunk struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *chatUsage        `json:"usage"`
	Error   *chatAPIError     `json:"error"`
}

type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        chatChunkDelta `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type chatChunkDelta struct {
	Role      string              `json:"role"`
	Content   string              `json:"content"`
	Refusal   string              `json:"refusal"`
	ToolCalls []chatChunkToolCall `json:"tool_calls"`
	// Copilot-specific reasoning fields.
	ReasoningText   string `json:"reasoning_text"`
	ReasoningOpaque string `json:"reasoning_opaque"`
}

type chatChunkToolCall struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function chatChunkToolCallFn `json:"function"`
}

type chatChunkToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// createChatStream issues a streaming chat-completions request and returns a
// reader that translates the SSE chunks into Anthropic-style stream events. It
// acquires the concurrency semaphore around request setup, so it must only be
// called by paths that do not already hold it (e.g. CreateMessageStream).
// Copilot exposes plaintext reasoning only on this path (message.reasoning_text);
// the /v1/messages shim returns signature-only thinking.
func (c *Client) createChatStream(ctx context.Context, req anthropic.CreateMessageRequest) (messageStream, error) {
	// Acquire the concurrency semaphore for the request setup, mirroring the
	// Responses streaming path. It is released once the reader is constructed.
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()
	return c.sendChatStream(ctx, req)
}

// sendChatStream builds and issues the streaming chat-completions request
// without touching the concurrency semaphore. Callers that already hold the
// semaphore — CreateMessage's doWithRetry, via createViaChatCompletions —
// reuse this path to buffer the streamed response, so re-acquiring the
// semaphore here would deadlock once in-flight requests reach the limit.
func (c *Client) sendChatStream(ctx context.Context, req anthropic.CreateMessageRequest) (messageStream, error) {
	chatReq, err := toChatRequest(req)
	if err != nil {
		return nil, err
	}
	chatReq.Models = withModelFallbacks(chatReq.Model, c.modelFallbacks)
	chatReq.Stream = true
	chatReq.StreamOptions = &chatStreamOptions{IncludeUsage: true}
	// Copilot uses the flat reasoning_effort field (not the nested OpenRouter
	// "reasoning" object) and only then returns reasoning_text.
	if isGitHubCopilotHost(hostFromURL(c.completionsURL)) {
		if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" {
			chatReq.ReasoningEffort = effort
			chatReq.Reasoning = nil
		}
	}

	data, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat stream request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.completionsURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponseBytes))
		resp.Body.Close()
		return nil, parseHTTPError(resp, raw, "chat_completions", c.completionsURL)
	}

	log.Printf("[openai] using api=chat_completions_stream url=%s", c.completionsURL)
	return newChatStreamReader(resp), nil
}

// chatStreamReader translates a streaming chat-completions SSE body into
// Anthropic-style StreamEvents, satisfying the messageStream interface.
//
// The downstream assembler (internal/anthropic.StreamAssembler) appends every
// delta and stop to the most recently started block builder, so blocks MUST be
// emitted strictly sequentially: at most one block is open at a time. Reasoning
// and text are streamed live under that invariant; tool calls are buffered by
// index and emitted as complete, sequential blocks in finalize().
type chatStreamReader struct {
	resp    *http.Response
	scanner *bufio.Scanner
	buf     []anthropic.StreamEvent

	messageStartSent bool
	messageStopSent  bool
	streamID         string
	streamModel      string

	nextIndex int

	openKind        int // chatBlockNone | chatBlockReasoning | chatBlockText
	openIndex       int
	reasoningOpaque string

	toolOrder []int
	toolAccs  map[int]*chatStreamToolAcc

	finishReason string
	usage        *chatUsage
}

const (
	chatBlockNone = iota
	chatBlockReasoning
	chatBlockText
)

type chatStreamToolAcc struct {
	id   string
	name string
	args strings.Builder
}

func newChatStreamReader(resp *http.Response) *chatStreamReader {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxProviderResponseBytes)
	return &chatStreamReader{
		resp:     resp,
		scanner:  scanner,
		toolAccs: make(map[int]*chatStreamToolAcc),
	}
}

func (r *chatStreamReader) Next() (anthropic.StreamEvent, error) {
	for {
		if len(r.buf) > 0 {
			ev := r.buf[0]
			r.buf = r.buf[1:]
			return ev, nil
		}
		if r.messageStopSent {
			return anthropic.StreamEvent{}, io.EOF
		}

		line, ok := r.readData()
		if !ok {
			// Surface read errors (truncated stream, oversized token) instead of
			// silently finalizing a partial response as if it succeeded.
			if err := r.scanner.Err(); err != nil {
				return anthropic.StreamEvent{}, fmt.Errorf("read chat stream: %w", err)
			}
			// Clean EOF without an explicit [DONE]; finalize gracefully.
			r.finalize()
			continue
		}
		if line == "[DONE]" {
			r.finalize()
			continue
		}

		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			// Skip malformed keep-alive or partial payloads.
			continue
		}
		if chunk.Error != nil {
			return anthropic.StreamEvent{}, &RequestError{StatusCode: 400, Body: chunk.Error.Message, API: "chat_completions"}
		}
		r.translateChunk(chunk)
	}
}

// readData returns the JSON payload of the next "data:" SSE line, or ok=false
// when the underlying stream is exhausted.
func (r *chatStreamReader) readData() (string, bool) {
	for r.scanner.Scan() {
		line := strings.TrimSpace(r.scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
	}
	return "", false
}

func (r *chatStreamReader) Close() error {
	if r.resp != nil && r.resp.Body != nil {
		return r.resp.Body.Close()
	}
	return nil
}

func (r *chatStreamReader) emit(ev anthropic.StreamEvent) {
	r.buf = append(r.buf, ev)
}

func (r *chatStreamReader) ensureMessageStart() {
	if r.messageStartSent {
		return
	}
	r.messageStartSent = true
	r.emit(anthropic.StreamEvent{
		Type: anthropic.EventMessageStart,
		Message: &anthropic.CreateMessageResponse{
			ID:    r.streamID,
			Model: r.streamModel,
			Role:  anthropic.RoleAssistant,
		},
	})
}

func (r *chatStreamReader) translateChunk(chunk chatCompletionChunk) {
	if chunk.ID != "" {
		r.streamID = chunk.ID
	}
	if chunk.Model != "" {
		r.streamModel = chunk.Model
	}
	if chunk.Usage != nil {
		r.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	r.ensureMessageStart()

	choice := chunk.Choices[0]
	delta := choice.Delta

	if delta.ReasoningOpaque != "" {
		r.reasoningOpaque = delta.ReasoningOpaque
	}

	if delta.ReasoningText != "" {
		if r.openKind != chatBlockReasoning {
			r.startBlock(chatBlockReasoning, anthropic.ContentBlock{Type: "thinking"})
		}
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockDelta,
			Index: r.openIndex,
			Delta: &anthropic.DeltaBlock{Type: "thinking_delta", Thinking: delta.ReasoningText, Text: delta.ReasoningText},
		})
	}

	if delta.Content != "" {
		if r.openKind != chatBlockText {
			r.startBlock(chatBlockText, anthropic.ContentBlock{Type: "text"})
		}
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockDelta,
			Index: r.openIndex,
			Delta: &anthropic.DeltaBlock{Type: "text_delta", Text: delta.Content},
		})
	}

	// Surface refusal deltas as text so the reason reaches the caller instead
	// of producing an empty (and wrongly retried) response.
	if delta.Refusal != "" {
		if r.openKind != chatBlockText {
			r.startBlock(chatBlockText, anthropic.ContentBlock{Type: "text"})
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockDelta,
				Index: r.openIndex,
				Delta: &anthropic.DeltaBlock{Type: "text_delta", Text: "The model refused to respond: "},
			})
		}
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockDelta,
			Index: r.openIndex,
			Delta: &anthropic.DeltaBlock{Type: "text_delta", Text: delta.Refusal},
		})
	}

	// Tool calls are buffered by provider index and only emitted, fully formed
	// and strictly sequential, in finalize(). This keeps the single-open-block
	// invariant even when arguments stream across chunks or arrive after text.
	for _, tc := range delta.ToolCalls {
		acc := r.toolAccs[tc.Index]
		if acc == nil {
			acc = &chatStreamToolAcc{}
			r.toolAccs[tc.Index] = acc
			r.toolOrder = append(r.toolOrder, tc.Index)
		}
		if tc.ID != "" {
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			acc.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			acc.args.WriteString(tc.Function.Arguments)
		}
	}

	if choice.FinishReason != "" {
		r.finishReason = choice.FinishReason
	}
}

// startBlock closes any currently open block and opens a new one at the next
// index, preserving the assembler's single-open-block requirement.
func (r *chatStreamReader) startBlock(kind int, block anthropic.ContentBlock) {
	r.closeOpenBlock()
	r.openIndex = r.nextIndex
	r.nextIndex++
	r.openKind = kind
	cb := block
	r.emit(anthropic.StreamEvent{
		Type:         anthropic.EventContentBlockStart,
		Index:        r.openIndex,
		ContentBlock: &cb,
	})
}

// closeOpenBlock emits the stop (and, for reasoning, the signature) for the
// currently open reasoning/text block, if any.
func (r *chatStreamReader) closeOpenBlock() {
	switch r.openKind {
	case chatBlockReasoning:
		if r.reasoningOpaque != "" {
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockDelta,
				Index: r.openIndex,
				Delta: &anthropic.DeltaBlock{Type: "signature_delta", Signature: r.reasoningOpaque},
			})
			// The signature belongs to the block being closed; a later reasoning
			// block must carry its own.
			r.reasoningOpaque = ""
		}
		r.emit(anthropic.StreamEvent{Type: anthropic.EventContentBlockStop, Index: r.openIndex})
	case chatBlockText:
		r.emit(anthropic.StreamEvent{Type: anthropic.EventContentBlockStop, Index: r.openIndex})
	}
	r.openKind = chatBlockNone
}

func (r *chatStreamReader) finalize() {
	if r.messageStopSent {
		return
	}
	r.ensureMessageStart()
	r.closeOpenBlock()

	for _, idx := range r.toolOrder {
		acc := r.toolAccs[idx]
		if acc == nil {
			continue
		}
		blockIdx := r.nextIndex
		r.nextIndex++
		r.emit(anthropic.StreamEvent{
			Type:  anthropic.EventContentBlockStart,
			Index: blockIdx,
			ContentBlock: &anthropic.ContentBlock{
				Type: "tool_use",
				ID:   acc.id,
				Name: acc.name,
			},
		})
		if args := acc.args.String(); args != "" {
			r.emit(anthropic.StreamEvent{
				Type:  anthropic.EventContentBlockDelta,
				Index: blockIdx,
				Delta: &anthropic.DeltaBlock{Type: "input_json_delta", PartialJSON: args},
			})
		}
		r.emit(anthropic.StreamEvent{Type: anthropic.EventContentBlockStop, Index: blockIdx})
	}

	usage := &anthropic.Usage{}
	if r.usage != nil {
		usage.InputTokens = r.usage.PromptTokens
		usage.OutputTokens = r.usage.CompletionTokens
	}
	r.emit(anthropic.StreamEvent{
		Type: anthropic.EventMessageDelta,
		Delta: &anthropic.DeltaBlock{
			Type:       "message_delta",
			StopReason: string(chatStopReason(r.finishReason, len(r.toolOrder) > 0)),
		},
		Usage: usage,
	})
	r.emit(anthropic.StreamEvent{Type: anthropic.EventMessageStop})
	r.messageStopSent = true
}
