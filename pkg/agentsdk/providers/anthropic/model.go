package anthropic

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// AnthropicProvider implements ModelProvider for the Anthropic API.
type AnthropicProvider struct {
	apiKey           string
	baseURL          string
	authMode         string
	bearerToken      string
	oauthTokenSource internalanthropic.TokenSource
	requestHeaders   func(context.Context) (map[string]string, error)
	adaptiveThinking bool
	promptCaching    bool
	defaultMaxTokens int
}

type ProviderConfig struct {
	APIKey   string
	BaseURL  string
	AuthMode string
	// BearerToken authenticates with "Authorization: Bearer <token>" without
	// Anthropic's x-api-key / oauth-beta headers. Used for Anthropic-compatible
	// gateways such as GitHub Copilot's /v1/messages endpoint.
	BearerToken string
	// OAuthTokenSource, when set with AuthMode "oauth", resolves the bearer
	// token per request so rotated or self-refreshed credentials (e.g. a
	// Kubernetes Secret mount managed by a central refresher) take effect
	// mid-run instead of pinning the startup token.
	OAuthTokenSource internalanthropic.TokenSource
	// RequestHeaders, when set, supplies per-request headers (gateway auth and
	// integration headers) via SDK middleware.
	RequestHeaders func(context.Context) (map[string]string, error)
	// AdaptiveThinking marks the deployment as effort-first: an
	// Anthropic-compatible gateway (e.g. GitHub Copilot's /v1/messages shim)
	// that controls reasoning via thinking.type=adaptive + output_config.effort
	// on every model generation that supports it. The shape is still resolved
	// per model: generations that only implement thinking.type=enabled +
	// budget_tokens (claude-*-4.5 and older) keep the enabled shape, which is
	// the only one that returns thinking blocks for them.
	AdaptiveThinking bool
	// PromptCaching enables Anthropic prompt-cache breakpoints: the tool
	// prefix, system prompt, and the last two conversation positions are
	// marked ephemeral so replayed agent context bills at the cache-read rate
	// (0.1x input) instead of full price.
	PromptCaching bool
	// DefaultMaxTokens overrides the built-in 16384 default for requests that
	// do not set Settings.MaxTokens (e.g. 64000 on Copilot's shim, which caps
	// rather than rejects oversized values).
	DefaultMaxTokens int
}

// NewAnthropicProvider creates a provider that must be configured with an API
// key before it can create models.
func NewAnthropicProvider() *AnthropicProvider {
	return &AnthropicProvider{}
}

func NewAnthropicProviderWithConfig(cfg ProviderConfig) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey:           strings.TrimSpace(cfg.APIKey),
		baseURL:          strings.TrimSpace(cfg.BaseURL),
		authMode:         strings.ToLower(strings.TrimSpace(cfg.AuthMode)),
		bearerToken:      strings.TrimSpace(cfg.BearerToken),
		oauthTokenSource: cfg.OAuthTokenSource,
		requestHeaders:   cfg.RequestHeaders,
		adaptiveThinking: cfg.AdaptiveThinking,
		promptCaching:    cfg.PromptCaching,
		defaultMaxTokens: cfg.DefaultMaxTokens,
	}
}

func (p *AnthropicProvider) GetModel(name string) (agentsdk.Model, error) {
	name = agentsdk.ResolveModelForProvider(name, "anthropic")
	m, err := newAnthropicModel(anthropicModelConfig{
		apiKey:           p.apiKey,
		baseURL:          p.baseURL,
		authMode:         p.authMode,
		bearerToken:      p.bearerToken,
		oauthTokenSource: p.oauthTokenSource,
		requestHeaders:   p.requestHeaders,
	})
	if err != nil {
		return nil, err
	}
	m.model = name
	m.adaptiveThinking = p.adaptiveThinking
	m.promptCaching = p.promptCaching
	m.defaultMaxTokens = p.defaultMaxTokens
	return m, nil
}

func (p *AnthropicProvider) Close() error { return nil }

// AnthropicModel implements Model using the Anthropic API.
type AnthropicModel struct {
	client           *internalanthropic.Client
	model            string
	oauth            bool
	adaptiveThinking bool
	promptCaching    bool
	defaultMaxTokens int

	// thinkingShape overrides the per-model thinking-shape heuristic after the
	// API rejected the derived shape with the thinking.type 400 (see
	// flipThinkingShapeOnError). 0 = auto, 1 = adaptive, 2 = enabled.
	thinkingShape atomic.Int32
}

type anthropicModelConfig struct {
	apiKey           string
	baseURL          string
	authMode         string
	bearerToken      string
	oauthTokenSource internalanthropic.TokenSource
	requestHeaders   func(context.Context) (map[string]string, error)
}

func newAnthropicModel(cfg anthropicModelConfig) (*AnthropicModel, error) {
	apiKey := strings.TrimSpace(cfg.apiKey)
	bearer := strings.TrimSpace(cfg.bearerToken)
	oauthMode := strings.EqualFold(cfg.authMode, "oauth")
	credential := apiKey
	if credential == "" {
		credential = bearer
	}
	if credential == "" && !(oauthMode && cfg.oauthTokenSource != nil) {
		return nil, &agentsdk.AgentError{Message: "Anthropic credential is required"}
	}
	var opts []internalanthropic.Option
	if cfg.baseURL != "" {
		opts = append(opts, internalanthropic.WithBaseURL(cfg.baseURL))
	}
	switch {
	case oauthMode:
		opts = append(opts, internalanthropic.WithOAuthToken(credential))
		if cfg.oauthTokenSource != nil {
			opts = append(opts, internalanthropic.WithOAuthTokenSource(cfg.oauthTokenSource))
		}
	case bearer != "":
		opts = append(opts, internalanthropic.WithBearerToken(bearer))
	}
	if cfg.requestHeaders != nil {
		opts = append(opts, internalanthropic.WithRequestHeaderProvider(cfg.requestHeaders))
	}
	client := internalanthropic.NewClient(credential, opts...)
	return &AnthropicModel{client: client, oauth: oauthMode}, nil
}

// NewAnthropicModelWithClient creates an AnthropicModel with an existing client.
func NewAnthropicModelWithClient(client *internalanthropic.Client) *AnthropicModel {
	return &AnthropicModel{client: client}
}

func (m *AnthropicModel) Provider() string { return "anthropic" }

func (m *AnthropicModel) GetResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelResponse, error) {
	if m == nil || m.client == nil {
		return nil, errors.New("anthropic model is not configured")
	}
	apiReq := m.buildRequest(req)
	resp, err := m.client.CreateMessage(ctx, apiReq)
	if err != nil {
		flipped, ok := m.flipThinkingShapeOnError(err, apiReq, req)
		if !ok {
			return nil, err
		}
		if resp, err = m.client.CreateMessage(ctx, flipped); err != nil {
			return nil, err
		}
	}
	return m.convertResponse(resp), nil
}

func (m *AnthropicModel) StreamResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelStream, error) {
	if m == nil || m.client == nil {
		return nil, errors.New("anthropic model is not configured")
	}
	apiReq := m.buildRequest(req)
	stream, err := m.client.CreateMessageStream(ctx, apiReq)
	if err != nil {
		flipped, ok := m.flipThinkingShapeOnError(err, apiReq, req)
		if !ok {
			return nil, err
		}
		if stream, err = m.client.CreateMessageStream(ctx, flipped); err != nil {
			return nil, err
		}
	}
	return m.wrapStream(stream), nil
}

func (m *AnthropicModel) GetRetryAdvice(err error) *agentsdk.ModelRetryAdvice {
	var reqErr *internalanthropic.RequestError
	if !errors.As(err, &reqErr) {
		return &agentsdk.ModelRetryAdvice{ShouldRetry: false}
	}
	return &agentsdk.ModelRetryAdvice{
		ShouldRetry:  reqErr.Retryable(),
		RetryAfterMS: int64(reqErr.RetryAfterMS()),
		Reason:       strconv.Itoa(reqErr.StatusCode),
	}
}

func (m *AnthropicModel) CalculateCost(usage agentsdk.Usage) float64 {
	return internalanthropic.CalculateCost(m.resolveModel(), internalanthropic.Usage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreateTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
	})
}

func (m *AnthropicModel) resolveModel() string {
	if m != nil && m.model != "" {
		return m.model
	}
	return "claude-sonnet-4-20250514"
}

func (m *AnthropicModel) buildRequest(req agentsdk.ModelRequest) internalanthropic.CreateMessageRequest {
	model := req.Model
	if model == "" {
		model = m.resolveModel()
	}

	apiReq := internalanthropic.CreateMessageRequest{
		Model: model,
		Betas: internalanthropic.ModelBetas(model),
	}
	if req.OutputSchema != nil {
		apiReq.OutputSchema = &internalanthropic.OutputSchema{
			Name:   req.OutputSchema.Name,
			Schema: req.OutputSchema.Schema,
			Strict: req.OutputSchema.Strict,
		}
	}

	// System prompt as SystemBlock slice. OAuth (Claude subscription) traffic
	// must look like Claude Code, whose system prompt always begins with the
	// fixed identity line; other OAuth integrations (pi-anthropic-oauth,
	// opencode) prepend the same block for compatibility.
	if m.oauth {
		apiReq.System = append(apiReq.System, internalanthropic.SystemBlock{
			Type: "text", Text: internalanthropic.ClaudeCodeIdentity,
		})
	}
	if req.Instructions != "" {
		apiReq.System = append(apiReq.System, internalanthropic.SystemBlock{
			Type: "text", Text: req.Instructions,
		})
	}

	if req.Settings.MaxTokens > 0 {
		apiReq.MaxTokens = req.Settings.MaxTokens
	} else if m.defaultMaxTokens > 0 {
		apiReq.MaxTokens = m.defaultMaxTokens
	} else {
		apiReq.MaxTokens = 16384
	}

	m.applyThinkingConfig(&apiReq, model, req.Settings)

	// Convert tools.
	for _, t := range req.Tools {
		apiReq.Tools = append(apiReq.Tools, internalanthropic.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}

	// Convert input items to messages.
	apiReq.Messages = itemsToAnthropicMessages(req.Input)

	if m.promptCaching {
		applyPromptCacheBreakpoints(&apiReq)
	}

	return apiReq
}

// Thinking-shape override states recorded after a thinking.type 400.
const (
	thinkingShapeAuto     int32 = iota // resolve per model
	thinkingShapeAdaptive              // force adaptive + output_config.effort
	thinkingShapeEnabled               // force enabled + budget_tokens
)

// applyThinkingConfig emits the extended-thinking request config in the shape
// the target model implements. Claude generations up to 4.5 only accept
// thinking.type=enabled + budget_tokens; the 4.6+/fable/5.x generations accept
// (and on effort-first gateways such as Copilot's /v1/messages shim, require)
// thinking.type=adaptive + output_config.effort. Sending the wrong shape either
// 400s or — on the Copilot shim's 4.5 family — silently returns no thinking
// blocks, which is what used to hide Claude reasoning on Copilot.
//
// Adaptive requests always pin display=summarized: the Messages API documents
// summarized as the default, but Copilot's shim behaves as display=omitted when
// the field is absent and returns signature-only thinking blocks with no text
// (verified live against claude-fable-5), which hides reasoning end-to-end.
func (m *AnthropicModel) applyThinkingConfig(apiReq *internalanthropic.CreateMessageRequest, model string, settings agentsdk.ModelSettings) {
	effort := mapReasoningEffortToAnthropic(settings.ReasoningEffort)
	if effort == "" && settings.ThinkingBudget <= 0 {
		// No reasoning requested (or explicitly "none" without a budget).
		return
	}
	if m.useAdaptiveThinking(model) {
		if effort == "" {
			effort = string(internalanthropic.OutputEffortMedium)
		}
		apiReq.Thinking = &internalanthropic.ThinkingConfig{Type: "adaptive", Display: "summarized"}
		apiReq.OutputEffort = effort
		return
	}
	budget := settings.ThinkingBudget
	if budget <= 0 {
		budget = thinkingBudgetForEffort(effort)
	}
	if budget <= 0 {
		return
	}
	apiReq.Thinking = &internalanthropic.ThinkingConfig{
		Type:         "enabled",
		BudgetTokens: budget,
	}
	apiReq.OutputEffort = ""
}

// useAdaptiveThinking picks the thinking request shape for a model. A shape
// recorded by flipThinkingShapeOnError wins; otherwise effort-first gateways
// (adaptiveThinking, e.g. Copilot's /v1/messages shim) use adaptive on every
// generation that supports it, and the first-party API keeps enabled +
// budget_tokens except on the models that reject it.
func (m *AnthropicModel) useAdaptiveThinking(model string) bool {
	switch m.thinkingShape.Load() {
	case thinkingShapeAdaptive:
		return true
	case thinkingShapeEnabled:
		return false
	}
	if m.adaptiveThinking {
		return internalanthropic.ModelSupportsAdaptiveThinking(model)
	}
	return internalanthropic.ModelRequiresAdaptiveThinking(model)
}

// thinkingBudgetForEffort converts a reasoning-effort label into a fixed
// thinking budget for models that only implement enabled + budget_tokens.
// The ladder mirrors agent.ModeReasoningSettings so an effort-only request
// behaves the same as the equivalent mode-level reasoning setting.
func thinkingBudgetForEffort(effort string) int {
	switch effort {
	case internalanthropic.OutputEffortLow:
		return 2048
	case internalanthropic.OutputEffortMedium:
		return 4096
	case internalanthropic.OutputEffortHigh:
		return 8192
	case internalanthropic.OutputEffortXHigh, internalanthropic.OutputEffortMax:
		return 12288
	default:
		return 0
	}
}

// flipThinkingShapeOnError rebuilds the request with the opposite thinking
// shape when the API rejected the current one, and records the working shape
// for the rest of the model's lifetime. The per-model generation split is a
// heuristic mirror of the provider catalogs, so a drifted deployment answers
// with HTTP 400 bodies like `"thinking.type.enabled" is not supported for this
// model. Use "thinking.type.adaptive" and "output_config.effort" ...` — the
// one-shot flip self-heals in both directions instead of failing the run.
func (m *AnthropicModel) flipThinkingShapeOnError(err error, sent internalanthropic.CreateMessageRequest, req agentsdk.ModelRequest) (internalanthropic.CreateMessageRequest, bool) {
	if sent.Thinking == nil {
		return sent, false
	}
	var reqErr *internalanthropic.RequestError
	if !errors.As(err, &reqErr) || reqErr.StatusCode != 400 {
		return sent, false
	}
	if !strings.Contains(strings.ToLower(reqErr.Body), "thinking.type") {
		return sent, false
	}
	if sent.Thinking.Type == "adaptive" {
		m.thinkingShape.Store(thinkingShapeEnabled)
	} else {
		m.thinkingShape.Store(thinkingShapeAdaptive)
	}
	return m.buildRequest(req), true
}

// applyPromptCacheBreakpoints marks the standard agent-loop cache boundaries
// with ephemeral cache_control (Anthropic allows at most 4 breakpoints):
// the last tool definition (caches the whole tool prefix), the last system
// block, and the last cacheable content block of the final two messages —
// a sliding window where the previous turn's write becomes this turn's read.
// Blocks below the provider's minimum cacheable size are ignored server-side,
// so unconditionally marking these boundaries is safe.
func applyPromptCacheBreakpoints(apiReq *internalanthropic.CreateMessageRequest) {
	ephemeral := &internalanthropic.CacheControl{Type: "ephemeral"}
	if n := len(apiReq.Tools); n > 0 {
		apiReq.Tools[n-1].CacheControl = ephemeral
	}
	if n := len(apiReq.System); n > 0 {
		apiReq.System[n-1].CacheControl = ephemeral
	}
	marked := 0
	for i := len(apiReq.Messages) - 1; i >= 0 && marked < 2; i-- {
		if markLastCacheableBlock(apiReq.Messages[i].Content, ephemeral) {
			marked++
		}
	}
}

// markLastCacheableBlock sets cache_control on the last block of a message
// that accepts it. Thinking, redacted_thinking, and compaction blocks cannot
// carry cache_control.
func markLastCacheableBlock(blocks []internalanthropic.ContentBlock, cc *internalanthropic.CacheControl) bool {
	for i := len(blocks) - 1; i >= 0; i-- {
		switch blocks[i].Type {
		case "text", "tool_result", "tool_use", "image", "document":
			blocks[i].CacheControl = cc
			return true
		}
	}
	return false
}

// mapReasoningEffortToAnthropic maps a host reasoning-effort label to a Messages
// API output_config.effort value. Returns "" when no effort is requested
// (including "none"), so callers can decide whether to fall back to a default.
//
// Anthropic's output_config.effort scale is low/medium/high/xhigh/max, but the
// supported subset is model-specific: e.g. claude-sonnet-4.6 accepts only
// [low medium high max] and rejects "xhigh" with HTTP 400. We therefore map the
// host's xhigh tier to Anthropic's canonical "max" effort. A first-class host
// max selection also remains max. The OpenAI path preserves xhigh and max as
// distinct efforts.
func mapReasoningEffortToAnthropic(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return internalanthropic.OutputEffortLow
	case "medium":
		return internalanthropic.OutputEffortMedium
	case "high":
		return internalanthropic.OutputEffortHigh
	case "xhigh", "max":
		return internalanthropic.OutputEffortMax
	default:
		return ""
	}
}

// itemsToAnthropicMessages converts RunItems to Anthropic message format.
func itemsToAnthropicMessages(items []agentsdk.RunItem) []internalanthropic.Message {
	var msgs []internalanthropic.Message
	for _, item := range items {
		switch item.Type {
		case agentsdk.RunItemMessage:
			if item.Message != nil {
				role := internalanthropic.RoleUser
				if item.Agent != nil {
					role = internalanthropic.RoleAssistant
				}
				var blocks []internalanthropic.ContentBlock
				if item.Message.Text != "" {
					blocks = append(blocks, internalanthropic.NewTextBlock(item.Message.Text))
				}
				for _, img := range item.Message.Images {
					if img.Data == "" {
						continue
					}
					if strings.EqualFold(strings.TrimSpace(img.MediaType), "application/pdf") {
						blocks = append(blocks, internalanthropic.NewDocumentBlock(img.MediaType, img.Data))
						continue
					}
					blocks = append(blocks, internalanthropic.NewImageBlock(img.MediaType, img.Data))
				}
				if len(blocks) > 0 {
					msgs = append(msgs, internalanthropic.Message{Role: role, Content: blocks})
				}
			}
		case agentsdk.RunItemToolCall:
			if item.ToolCall != nil {
				msgs = append(msgs, internalanthropic.Message{
					Role: internalanthropic.RoleAssistant,
					Content: []internalanthropic.ContentBlock{
						internalanthropic.NewToolUseBlock(item.ToolCall.ID, item.ToolCall.Name, item.ToolCall.Input),
					},
				})
			}
		case agentsdk.RunItemToolOutput:
			if item.ToolOutput != nil {
				msgs = append(msgs, internalanthropic.Message{
					Role: internalanthropic.RoleUser,
					Content: []internalanthropic.ContentBlock{
						internalanthropic.NewToolResultBlock(item.ToolOutput.CallID, item.ToolOutput.Content, item.ToolOutput.IsError),
					},
				})
			}
		case agentsdk.RunItemReasoning:
			if item.Reasoning != nil {
				block := internalanthropic.NewThinkingBlock(item.Reasoning.Text)
				block.ID = item.Reasoning.ID
				block.Signature = item.Reasoning.Signature
				block.EncryptedContent = item.Reasoning.EncryptedContent
				if item.Reasoning.RedactedData != "" && item.Reasoning.Text == "" {
					block = internalanthropic.NewRedactedThinkingBlock(item.Reasoning.RedactedData)
					block.ID = item.Reasoning.ID
					block.EncryptedContent = item.Reasoning.EncryptedContent
				}
				msgs = append(msgs, internalanthropic.Message{
					Role: internalanthropic.RoleAssistant,
					Content: []internalanthropic.ContentBlock{
						block,
					},
				})
			}
		case agentsdk.RunItemCompaction:
			if item.Compaction != nil && strings.TrimSpace(item.Compaction.EncryptedContent) != "" {
				// Encrypted compaction blobs are decryptable only by the
				// provider that produced them. After a cross-provider model
				// fallback/switch, forwarding e.g. an OpenAI blob to the
				// Anthropic API yields a hard 400. The blob is the only
				// remnant of the history pruned behind it, so down-convert
				// known-foreign blobs to a plaintext assistant summary when
				// the producing provider supplied one instead of silently
				// severing the conversation.
				if origin := strings.ToLower(strings.TrimSpace(item.Compaction.CreatedBy)); origin != "" && origin != "anthropic" {
					if summary := strings.TrimSpace(item.Compaction.Content); summary != "" {
						msgs = append(msgs, internalanthropic.Message{
							Role:    internalanthropic.RoleAssistant,
							Content: []internalanthropic.ContentBlock{internalanthropic.NewTextBlock(internalanthropic.ForeignCompactionSummaryHeader + summary)},
						})
					}
					continue
				}
				block := internalanthropic.NewCompactionBlock(item.Compaction.ID, item.Compaction.EncryptedContent, item.Compaction.CreatedBy)
				block.Content = item.Compaction.Content
				msgs = append(msgs, internalanthropic.Message{
					Role:    internalanthropic.RoleAssistant,
					Content: []internalanthropic.ContentBlock{block},
				})
			}
		}
	}
	return msgs
}

func (m *AnthropicModel) convertResponse(resp *internalanthropic.CreateMessageResponse) *agentsdk.ModelResponse {
	var items []agentsdk.RunItem
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			items = append(items, agentsdk.RunItem{
				Type:    agentsdk.RunItemMessage,
				Message: &agentsdk.MessageOutput{Text: block.Text},
			})
		case "tool_use":
			items = append(items, agentsdk.RunItem{
				Type: agentsdk.RunItemToolCall,
				ToolCall: &agentsdk.ToolCallData{
					ID:    block.ID,
					Name:  block.Name,
					Input: block.Input,
				},
			})
		case "thinking":
			items = append(items, agentsdk.RunItem{
				Type: agentsdk.RunItemReasoning,
				Reasoning: &agentsdk.ReasoningData{
					ID:               block.ID,
					Text:             block.Thinking,
					Signature:        block.Signature,
					EncryptedContent: block.EncryptedContent,
				},
			})
		case "redacted_thinking":
			items = append(items, agentsdk.RunItem{
				Type: agentsdk.RunItemReasoning,
				Reasoning: &agentsdk.ReasoningData{
					ID:               block.ID,
					RedactedData:     block.Data,
					EncryptedContent: block.EncryptedContent,
				},
			})
		case "compaction":
			items = append(items, agentsdk.RunItem{
				Type: agentsdk.RunItemCompaction,
				Compaction: &agentsdk.CompactionData{
					ID:               block.ID,
					Content:          block.Content,
					EncryptedContent: block.EncryptedContent,
					CreatedBy:        block.CreatedBy,
				},
			})
		}
	}

	return &agentsdk.ModelResponse{
		Items: items,
		Usage: agentsdk.Usage{
			Requests:          1,
			InputTokens:       resp.Usage.InputTokens,
			OutputTokens:      resp.Usage.OutputTokens,
			CacheReadTokens:   resp.Usage.CacheReadInputTokens,
			CacheCreateTokens: resp.Usage.CacheCreationInputTokens,
		},
		Raw: resp,
	}
}

func (m *AnthropicModel) wrapStream(reader *internalanthropic.StreamReader) *agentsdk.ModelStream {
	events := make(chan agentsdk.ModelStreamEvent, 64)
	done := make(chan *agentsdk.ModelResponse, 1)

	go func() {
		defer close(events)
		defer close(done)
		defer reader.Close()

		assembler := internalanthropic.NewStreamAssembler()

		for {
			evt, err := reader.Next()
			if err != nil {
				if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF") {
					break
				}
				events <- agentsdk.ModelStreamEvent{Type: agentsdk.ModelStreamError, Error: err}
				done <- nil
				return
			}
			assembler.Add(evt)
			switch evt.Type {
			case internalanthropic.EventContentBlockDelta:
				if evt.Delta != nil {
					switch evt.Delta.Type {
					case "text_delta":
						events <- agentsdk.ModelStreamEvent{
							Type:  agentsdk.ModelStreamDelta,
							Delta: evt.Delta.Text,
						}
					case "thinking_delta":
						events <- agentsdk.ModelStreamEvent{
							Type:  agentsdk.ModelStreamReasoningDelta,
							Delta: evt.Delta.Thinking,
						}
					}
				}
			case internalanthropic.EventMessageStop:
				// Stream finished.
			}
		}
		done <- m.convertResponse(assembler.Response())
	}()

	return agentsdk.NewModelStream(events, done)
}
