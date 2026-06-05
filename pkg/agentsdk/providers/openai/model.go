package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"strconv"
	"strings"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	internalopenai "github.com/gratefulagents/sdk/internal/openai"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

type OpenAIProvider struct {
	baseURL        string                  // optional override (for gemini, openrouter, groq)
	apiKey         string                  // optional API key override
	authMode       internalopenai.AuthMode // "oauth" or "" (api-key)
	apiMode        string                  // optional endpoint mode override
	authSession    *internalopenai.OpenAIAuthSession
	modelFallbacks []string // ordered OpenRouter-style fallback models
}

type ProviderConfig struct {
	BaseURL     string
	APIKey      string
	AuthMode    AuthMode
	APIMode     string
	AuthSession *AuthSession
	// ModelFallbacks is an ordered list of fallback model identifiers sent as
	// the OpenRouter "models" array so the provider retries the next model when
	// one is unavailable. Empty disables fallback routing.
	ModelFallbacks []string
}

func NewOpenAIProvider() *OpenAIProvider {
	return &OpenAIProvider{}
}

func NewOpenAIProviderWithBaseURL(baseURL string) *OpenAIProvider {
	return &OpenAIProvider{baseURL: baseURL}
}

func NewOpenAIProviderWithConfig(cfg ProviderConfig) *OpenAIProvider {
	return &OpenAIProvider{
		baseURL:        strings.TrimSpace(cfg.BaseURL),
		apiKey:         strings.TrimSpace(cfg.APIKey),
		authMode:       cfg.AuthMode,
		apiMode:        strings.TrimSpace(cfg.APIMode),
		authSession:    cfg.AuthSession,
		modelFallbacks: append([]string(nil), cfg.ModelFallbacks...),
	}
}

func (p *OpenAIProvider) GetModel(name string) (agentsdk.Model, error) {
	name = agentsdk.ResolveModelForProvider(name, "openai")
	m, err := newOpenAIModelFromConfig(p.baseURL, p.authMode, p.apiMode, p.apiKey, p.authSession, p.modelFallbacks)
	if err != nil {
		return nil, err
	}
	m.model = name
	return m, nil
}

func (p *OpenAIProvider) Close() error { return nil }

type OpenAIModel struct {
	client *internalopenai.Client
	model  string
}

func newOpenAIModelFromConfig(baseURL string, authMode internalopenai.AuthMode, apiMode string, apiKey string, session *internalopenai.OpenAIAuthSession, modelFallbacks []string) (*OpenAIModel, error) {
	var opts []internalopenai.Option
	baseURL = strings.TrimSpace(baseURL)
	if baseURL != "" {
		// Fail closed on a malformed custom endpoint instead of silently
		// falling back to the default OpenAI host, which would ship prompts
		// and credentials to the wrong provider.
		if u, err := url.Parse(baseURL); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, &agentsdk.AgentError{Message: fmt.Sprintf("invalid OpenAI base URL %q: must include scheme and host", baseURL)}
		}
		opts = append(opts, internalopenai.WithBaseURL(baseURL))
	}
	apiMode = strings.TrimSpace(apiMode)
	if apiMode != "" {
		opts = append(opts, internalopenai.WithAPIMode(apiMode))
	}
	if len(modelFallbacks) > 0 {
		opts = append(opts, internalopenai.WithModelFallbacks(modelFallbacks))
	}

	if authMode == "" {
		authMode = internalopenai.AuthModeAPIKey
	}

	switch {
	case session != nil:
	case strings.TrimSpace(apiKey) != "":
		session = internalopenai.NewAPIKeyAuthSession(apiKey)
	case authMode == internalopenai.AuthModeOAuth:
		return nil, &agentsdk.AgentError{Message: "OpenAI OAuth auth session is required"}
	default:
		return nil, &agentsdk.AgentError{Message: "OpenAI API key is required"}
	}

	return &OpenAIModel{client: internalopenai.NewClientWithAuthSession(session, opts...)}, nil
}

func NewOpenAIModelWithClient(client *internalopenai.Client) *OpenAIModel {
	return &OpenAIModel{client: client}
}

func (m *OpenAIModel) Provider() string { return "openai" }

func (m *OpenAIModel) SupportsContextCompaction() bool {
	return m != nil && m.client != nil && m.client.SupportsResponseCompaction()
}

func (m *OpenAIModel) GetResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelResponse, error) {
	if m == nil || m.client == nil {
		return nil, errors.New("openai model is not configured")
	}
	apiReq := m.buildRequest(req)
	resp, err := m.client.CreateMessage(ctx, apiReq)
	if err != nil {
		return nil, err
	}
	mr := convertAnthropicResponse(resp)

	var textCount, toolCount, thinkingCount int
	for _, item := range mr.Items {
		switch item.Type {
		case agentsdk.RunItemMessage:
			textCount++
		case agentsdk.RunItemToolCall:
			toolCount++
		case agentsdk.RunItemReasoning:
			thinkingCount++
		}
	}
	log.Printf("[openai] response: model=%s items=%d (text=%d tools=%d thinking=%d) usage=in:%d/out:%d stop=%s",
		resp.Model, len(mr.Items), textCount, toolCount, thinkingCount,
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.StopReason)

	return mr, nil
}

// AnalyzeImage analyzes a loaded image with the default high-detail Responses
// image-input path.
func (m *OpenAIModel) AnalyzeImage(ctx context.Context, imageData []byte, mimeType, prompt string) (string, error) {
	return m.AnalyzeImageWithDetail(ctx, imageData, mimeType, prompt, "high")
}

// AnalyzeImageWithDetail analyzes a loaded image using OpenAI Responses image
// input. It intentionally uses the model resolved on this OpenAIModel, so SDK
// callers can pin the vision model to gpt-5.5 independently of the agent model.
func (m *OpenAIModel) AnalyzeImageWithDetail(ctx context.Context, imageData []byte, mimeType, prompt, detail string) (string, error) {
	if m == nil || m.client == nil {
		return "", errors.New("openai model is not configured")
	}
	if len(imageData) == 0 {
		return "", errors.New("image data is empty")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("prompt is required")
	}
	resp, err := m.client.AnalyzeImage(ctx, m.resolveModel(), dataURLForImage(imageData, mimeType), prompt, detail)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(anthropicText(resp))
	if text == "" {
		return "", fmt.Errorf("openai vision response contained no text")
	}
	return text, nil
}

func (m *OpenAIModel) CompactContext(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.CompactionResult, error) {
	if !m.SupportsContextCompaction() {
		return nil, errors.New("openai responses compaction is not supported for this client")
	}
	apiReq := m.buildRequest(req)
	resp, err := m.client.CompactConversation(ctx, apiReq)
	if err != nil {
		return nil, err
	}
	items := anthropicMessagesToRunItems(resp.Messages)
	return &agentsdk.CompactionResult{
		Items: items,
		Usage: agentsdk.Usage{
			Requests:          1,
			InputTokens:       resp.Usage.InputTokens,
			OutputTokens:      resp.Usage.OutputTokens,
			CacheReadTokens:   resp.Usage.CacheReadInputTokens,
			CacheCreateTokens: resp.Usage.CacheCreationInputTokens,
		},
		Raw:     resp.Raw,
		Summary: summarizeProviderCompactionResponse(resp.ID, resp.Raw, items),
	}, nil
}

func (m *OpenAIModel) StreamResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelStream, error) {
	if m == nil || m.client == nil {
		return nil, errors.New("openai model is not configured")
	}
	apiReq := m.buildRequest(req)
	stream, err := m.client.CreateMessageStream(ctx, apiReq)
	if err != nil {
		return nil, err
	}
	return wrapAnthropicStyleStream(stream), nil
}

func (m *OpenAIModel) GetRetryAdvice(err error) *agentsdk.ModelRetryAdvice {
	var reqErr *internalopenai.RequestError
	if errors.As(err, &reqErr) {
		return &agentsdk.ModelRetryAdvice{
			ShouldRetry:  reqErr.Retryable(),
			RetryAfterMS: int64(reqErr.RetryAfterMS()),
			Reason:       strconv.Itoa(reqErr.StatusCode),
		}
	}
	var responseFailed *internalopenai.ResponseFailedError
	if errors.As(err, &responseFailed) {
		return &agentsdk.ModelRetryAdvice{
			ShouldRetry: responseFailed.Retryable(),
			Reason:      responseFailed.NonRetryableReason(),
		}
	}
	return &agentsdk.ModelRetryAdvice{ShouldRetry: false}
}

func (m *OpenAIModel) EstimateCost(usage agentsdk.Usage) (float64, bool) {
	return internalopenai.EstimateCost(m.resolveModel(), internalanthropic.Usage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
		CacheCreationInputTokens: usage.CacheCreateTokens,
	})
}

func (m *OpenAIModel) CalculateCost(usage agentsdk.Usage) float64 {
	cost, _ := m.EstimateCost(usage)
	return cost
}

func (m *OpenAIModel) resolveModel() string {
	if m != nil && m.model != "" {
		return m.model
	}
	return internalopenai.DefaultChatModel
}

func (m *OpenAIModel) buildRequest(req agentsdk.ModelRequest) internalanthropic.CreateMessageRequest {
	model := req.Model
	if model == "" {
		model = m.resolveModel()
	}

	apiReq := internalanthropic.CreateMessageRequest{
		Model: model,
	}
	if req.OutputSchema != nil {
		apiReq.OutputSchema = &internalanthropic.OutputSchema{
			Name:   req.OutputSchema.Name,
			Schema: req.OutputSchema.Schema,
			Strict: req.OutputSchema.Strict,
		}
	}
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

	if req.Settings.ThinkingBudget > 0 {
		apiReq.Thinking = &internalanthropic.ThinkingConfig{
			Type:         "enabled",
			BudgetTokens: req.Settings.ThinkingBudget,
		}
	} else if strings.EqualFold(req.Settings.ReasoningEffort, "minimal") {
		// OpenAI supports an explicit "minimal" effort, but this shim forwards
		// Anthropic-style thinking budgets. A tiny sentinel budget maps to
		// explicit minimal reasoning without enabling heavy thinking.
		apiReq.Thinking = &internalanthropic.ThinkingConfig{
			Type:         "enabled",
			BudgetTokens: 1,
		}
	}
	apiReq.TextVerbosity = req.Settings.TextVerbosity
	if req.CompactionThreshold > 0 && m.SupportsContextCompaction() {
		apiReq.CompactionThreshold = req.CompactionThreshold
	}

	for _, t := range req.Tools {
		apiReq.Tools = append(apiReq.Tools, internalanthropic.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}

	apiReq.Messages = itemsToAnthropicMessages(req.Input)

	return apiReq
}

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
					Role:    internalanthropic.RoleAssistant,
					Content: []internalanthropic.ContentBlock{block},
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

func convertAnthropicResponse(resp *internalanthropic.CreateMessageResponse) *agentsdk.ModelResponse {
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

func dataURLForImage(imageData []byte, mimeType string) string {
	mimeType = strings.TrimSpace(strings.Split(mimeType, ";")[0])
	if mimeType == "" || strings.ContainsAny(mimeType, " \t\r\n,") {
		mimeType = "application/octet-stream"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(imageData)
}

func anthropicText(resp *internalanthropic.CreateMessageResponse) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, block := range resp.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func anthropicMessagesToRunItems(messages []internalanthropic.Message) []agentsdk.RunItem {
	var items []agentsdk.RunItem
	assistantAgent := &agentsdk.Agent{Name: "assistant"}
	for _, msg := range messages {
		var agent *agentsdk.Agent
		if msg.Role == internalanthropic.RoleAssistant {
			agent = assistantAgent
		}
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				items = append(items, agentsdk.RunItem{
					Type:    agentsdk.RunItemMessage,
					Agent:   agent,
					Message: &agentsdk.MessageOutput{Text: block.Text},
				})
			case "tool_use":
				items = append(items, agentsdk.RunItem{
					Type:  agentsdk.RunItemToolCall,
					Agent: assistantAgent,
					ToolCall: &agentsdk.ToolCallData{
						ID:    block.ID,
						Name:  block.Name,
						Input: block.Input,
					},
				})
			case "tool_result":
				items = append(items, agentsdk.RunItem{
					Type: agentsdk.RunItemToolOutput,
					ToolOutput: &agentsdk.ToolOutputData{
						CallID:  block.ToolUseID,
						Content: block.Content,
						IsError: block.IsError,
					},
				})
			case "thinking", "redacted_thinking":
				reasoning := &agentsdk.ReasoningData{
					ID:               block.ID,
					Text:             block.Thinking,
					Signature:        block.Signature,
					RedactedData:     block.Data,
					EncryptedContent: block.EncryptedContent,
				}
				items = append(items, agentsdk.RunItem{Type: agentsdk.RunItemReasoning, Agent: assistantAgent, Reasoning: reasoning})
			case "compaction":
				items = append(items, agentsdk.RunItem{
					Type:  agentsdk.RunItemCompaction,
					Agent: assistantAgent,
					Compaction: &agentsdk.CompactionData{
						ID:               block.ID,
						EncryptedContent: block.EncryptedContent,
						CreatedBy:        block.CreatedBy,
					},
				})
			}
		}
	}
	return items
}

func summarizeProviderCompactionOutput(id string, items []agentsdk.RunItem) string {
	type outputItem struct {
		Type             string `json:"type"`
		Role             string `json:"role,omitempty"`
		ID               string `json:"id,omitempty"`
		Text             string `json:"text,omitempty"`
		EncryptedContent string `json:"encrypted_content,omitempty"`
		CreatedBy        string `json:"created_by,omitempty"`
	}
	out := struct {
		Type  string       `json:"type"`
		ID    string       `json:"id,omitempty"`
		Items []outputItem `json:"items"`
	}{
		Type: "openai_responses_compaction",
		ID:   id,
	}
	for _, item := range items {
		switch item.Type {
		case agentsdk.RunItemMessage:
			if item.Message == nil {
				continue
			}
			role := "assistant"
			if item.Agent == nil {
				role = "user"
			}
			out.Items = append(out.Items, outputItem{
				Type: "message",
				Role: role,
				Text: item.Message.Text,
			})
		case agentsdk.RunItemCompaction:
			if item.Compaction == nil {
				continue
			}
			out.Items = append(out.Items, outputItem{
				Type:             "compaction",
				ID:               item.Compaction.ID,
				EncryptedContent: item.Compaction.EncryptedContent,
				CreatedBy:        item.Compaction.CreatedBy,
			})
		case agentsdk.RunItemToolCall:
			if item.ToolCall != nil {
				out.Items = append(out.Items, outputItem{Type: "tool_call", ID: item.ToolCall.ID, Text: item.ToolCall.Name})
			}
		case agentsdk.RunItemToolOutput:
			if item.ToolOutput != nil {
				out.Items = append(out.Items, outputItem{Type: "tool_output", ID: item.ToolOutput.CallID, Text: item.ToolOutput.Content})
			}
		case agentsdk.RunItemReasoning:
			if item.Reasoning != nil {
				out.Items = append(out.Items, outputItem{Type: "reasoning", ID: item.Reasoning.ID, Text: item.Reasoning.Text, EncryptedContent: item.Reasoning.EncryptedContent})
			}
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "[OPENAI RESPONSES COMPACTION]"
	}
	return "[OPENAI RESPONSES COMPACTION]\n" + string(data)
}

type rawJSONProvider interface {
	RawJSON() string
}

func summarizeProviderCompactionResponse(id string, raw any, items []agentsdk.RunItem) string {
	if raw != nil {
		if provider, ok := raw.(rawJSONProvider); ok {
			if formatted := formatRawCompactionJSON(provider.RawJSON()); formatted != "" {
				return "[OPENAI RESPONSES COMPACTION]\n" + formatted
			}
		}
		if data, err := json.MarshalIndent(raw, "", "  "); err == nil && strings.TrimSpace(string(data)) != "" && strings.TrimSpace(string(data)) != "null" {
			return "[OPENAI RESPONSES COMPACTION]\n" + string(data)
		}
	}
	return summarizeProviderCompactionOutput(id, items)
}

func formatRawCompactionJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(raw), "", "  "); err == nil {
		return pretty.String()
	}
	return raw
}

// anthropicStyleStream is the interface satisfied by both internalanthropic.StreamReader
// and internalopenai.ResponsesStreamReader / internalopenai.StreamReader.
type anthropicStyleStream interface {
	Next() (internalanthropic.StreamEvent, error)
	Close() error
}

// wrapAnthropicStyleStream wraps any Anthropic-style stream (from either provider)
// into a agentsdk.ModelStream.
func wrapAnthropicStyleStream(stream anthropicStyleStream) *agentsdk.ModelStream {
	events := make(chan agentsdk.ModelStreamEvent, 64)
	done := make(chan *agentsdk.ModelResponse, 1)

	go func() {
		defer close(events)
		defer close(done)
		defer stream.Close()

		assembler := internalanthropic.NewStreamAssembler()

		for {
			evt, err := stream.Next()
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
							Type:  agentsdk.ModelStreamDelta,
							Delta: evt.Delta.Thinking,
						}
					}
				}
			case internalanthropic.EventMessageStop:
				// Stream finished.
			}
		}
		done <- convertAnthropicResponse(assembler.Response())
	}()

	return agentsdk.NewModelStream(events, done)
}
