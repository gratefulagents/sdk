package anthropic

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// AnthropicProvider implements ModelProvider for the Anthropic API.
type AnthropicProvider struct {
	apiKey           string
	baseURL          string
	authMode         string
	bearerToken      string
	requestHeaders   func(context.Context) (map[string]string, error)
	adaptiveThinking bool
}

type ProviderConfig struct {
	APIKey   string
	BaseURL  string
	AuthMode string
	// BearerToken authenticates with "Authorization: Bearer <token>" without
	// Anthropic's x-api-key / oauth-beta headers. Used for Anthropic-compatible
	// gateways such as GitHub Copilot's /v1/messages endpoint.
	BearerToken string
	// RequestHeaders, when set, supplies per-request headers (gateway auth and
	// integration headers) via SDK middleware.
	RequestHeaders func(context.Context) (map[string]string, error)
	// AdaptiveThinking makes the provider emit adaptive thinking
	// (thinking.type=adaptive + output_config.effort) instead of a fixed
	// thinking-budget. Required by GitHub Copilot's /v1/messages shim for newer
	// Claude models, which reject thinking.type=enabled.
	AdaptiveThinking bool
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
		requestHeaders:   cfg.RequestHeaders,
		adaptiveThinking: cfg.AdaptiveThinking,
	}
}

func (p *AnthropicProvider) GetModel(name string) (agentsdk.Model, error) {
	name = agentsdk.ResolveModelForProvider(name, "anthropic")
	m, err := newAnthropicModel(anthropicModelConfig{
		apiKey:         p.apiKey,
		baseURL:        p.baseURL,
		authMode:       p.authMode,
		bearerToken:    p.bearerToken,
		requestHeaders: p.requestHeaders,
	})
	if err != nil {
		return nil, err
	}
	m.model = name
	m.adaptiveThinking = p.adaptiveThinking
	return m, nil
}

func (p *AnthropicProvider) Close() error { return nil }

// AnthropicModel implements Model using the Anthropic API.
type AnthropicModel struct {
	client           *internalanthropic.Client
	model            string
	adaptiveThinking bool
}

type anthropicModelConfig struct {
	apiKey         string
	baseURL        string
	authMode       string
	bearerToken    string
	requestHeaders func(context.Context) (map[string]string, error)
}

func newAnthropicModel(cfg anthropicModelConfig) (*AnthropicModel, error) {
	apiKey := strings.TrimSpace(cfg.apiKey)
	bearer := strings.TrimSpace(cfg.bearerToken)
	credential := apiKey
	if credential == "" {
		credential = bearer
	}
	if credential == "" {
		return nil, &agentsdk.AgentError{Message: "Anthropic credential is required"}
	}
	var opts []internalanthropic.Option
	if cfg.baseURL != "" {
		opts = append(opts, internalanthropic.WithBaseURL(cfg.baseURL))
	}
	switch {
	case strings.EqualFold(cfg.authMode, "oauth"):
		opts = append(opts, internalanthropic.WithOAuthToken(credential))
	case bearer != "":
		opts = append(opts, internalanthropic.WithBearerToken(bearer))
	}
	if cfg.requestHeaders != nil {
		opts = append(opts, internalanthropic.WithRequestHeaderProvider(cfg.requestHeaders))
	}
	client := internalanthropic.NewClient(credential, opts...)
	return &AnthropicModel{client: client}, nil
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
		return nil, err
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
		return nil, err
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

	// System prompt as SystemBlock slice.
	if req.Instructions != "" {
		apiReq.System = []internalanthropic.SystemBlock{
			{Type: "text", Text: req.Instructions},
		}
	}

	if req.Settings.MaxTokens > 0 {
		apiReq.MaxTokens = req.Settings.MaxTokens
	} else {
		apiReq.MaxTokens = 16384
	}

	if m.adaptiveThinking {
		// Gateways such as Copilot's /v1/messages shim reject thinking.type=enabled
		// for newer Claude models and instead control reasoning via adaptive
		// thinking + output_config.effort. Emit that shape whenever reasoning is
		// requested (a thinking budget or an explicit effort).
		if effort := mapReasoningEffortToAnthropic(req.Settings.ReasoningEffort); effort != "" {
			apiReq.Thinking = &internalanthropic.ThinkingConfig{Type: "adaptive"}
			apiReq.OutputEffort = effort
		} else if req.Settings.ThinkingBudget > 0 {
			apiReq.Thinking = &internalanthropic.ThinkingConfig{Type: "adaptive"}
			apiReq.OutputEffort = string(internalanthropic.OutputEffortMedium)
		}
	} else if req.Settings.ThinkingBudget > 0 {
		apiReq.Thinking = &internalanthropic.ThinkingConfig{
			Type:         "enabled",
			BudgetTokens: req.Settings.ThinkingBudget,
		}
	}

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

	return apiReq
}

// mapReasoningEffortToAnthropic maps a host reasoning-effort label to a Messages
// API output_config.effort value. Returns "" when no effort is requested
// (including "none"), so callers can decide whether to fall back to a default.
//
// The host reasoning ladder tops out at "xhigh" ("extra reasoning for hardest
// tasks"). Anthropic's output_config.effort scale is low/medium/high/xhigh/max,
// but the supported subset is model-specific: e.g. claude-sonnet-4.6 accepts
// only [low medium high max] and rejects "xhigh" with HTTP 400. We therefore map
// the host's top tier to Anthropic's "max", which is the canonical maximum
// effort and is supported across Claude models, instead of the model-specific
// "xhigh". This keeps the OpenAI path (which does support "xhigh") unchanged.
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
				msgs = append(msgs, internalanthropic.Message{
					Role: internalanthropic.RoleAssistant,
					Content: []internalanthropic.ContentBlock{
						internalanthropic.NewCompactionBlock(item.Compaction.ID, item.Compaction.EncryptedContent, item.Compaction.CreatedBy),
					},
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
