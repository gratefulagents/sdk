package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/internal/anthropic"
	"github.com/openai/openai-go/v3/responses"
)

func TestToAnthropicResponse_ReconstructsToolCalls(t *testing.T) {
	resp := &responses.Response{
		ID:    "resp_test",
		Model: "gpt-5.4",
		Output: []responses.ResponseOutputItemUnion{
			{
				Type:  "message",
				Phase: responses.ResponseOutputMessagePhaseCommentary,
				Content: []responses.ResponseOutputMessageContentUnion{
					{Type: "output_text", Text: "Working..."},
				},
			},
			{
				Type:      "function_call",
				CallID:    "call_123",
				Name:      "Bash",
				Arguments: responses.ResponseOutputItemUnionArguments{OfString: `{"command":"echo hello"}`},
			},
		},
		Usage: responses.ResponseUsage{
			InputTokens:  11,
			OutputTokens: 7,
			InputTokensDetails: responses.ResponseUsageInputTokensDetails{
				CachedTokens: 2,
			},
		},
	}

	got, err := toAnthropicResponseFromResponses(resp)
	if err != nil {
		t.Fatalf("toAnthropicResponseFromResponses() error = %v", err)
	}

	if got.StopReason != anthropic.StopReasonToolUse {
		t.Fatalf("StopReason = %q, want %q", got.StopReason, anthropic.StopReasonToolUse)
	}
	if got.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want %q", got.Model, "gpt-5.4")
	}
	if got.Usage.InputTokens != 11 || got.Usage.OutputTokens != 7 || got.Usage.CacheReadInputTokens != 2 {
		t.Fatalf("Usage = %+v, want input=11 output=7 cached=2", got.Usage)
	}
	if len(got.Content) != 2 {
		t.Fatalf("Content len = %d, want 2", len(got.Content))
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "Working..." {
		t.Fatalf("text block = %+v, want text \"Working...\"", got.Content[0])
	}
	if got.Content[0].Phase != "commentary" {
		t.Fatalf("text block phase = %q, want commentary", got.Content[0].Phase)
	}
	if got.Content[1].Type != "tool_use" {
		t.Fatalf("second block type = %q, want tool_use", got.Content[1].Type)
	}
	if got.Content[1].ID != "call_123" {
		t.Fatalf("tool id = %q, want %q", got.Content[1].ID, "call_123")
	}
	if got.Content[1].Name != "Bash" {
		t.Fatalf("tool name = %q, want %q", got.Content[1].Name, "Bash")
	}
	if string(got.Content[1].Input) != `{"command":"echo hello"}` {
		t.Fatalf("tool args = %s, want %s", string(got.Content[1].Input), `{"command":"echo hello"}`)
	}
}

func TestToAnthropicResponseRejectsEmptyOutput(t *testing.T) {
	_, err := toAnthropicResponseFromResponses(&responses.Response{
		ID:     "resp_empty",
		Status: responses.ResponseStatusCompleted,
	})
	if err == nil || !strings.Contains(err.Error(), "no output content") {
		t.Fatalf("error = %v, want no output content", err)
	}
	var failed *ResponseFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error type = %T, want ResponseFailedError", err)
	}
	if !failed.Retryable() {
		t.Fatalf("empty output under a non-terminal status should be retryable: %+v", failed)
	}
}

func TestToAnthropicResponsePreservesFailedResponseReason(t *testing.T) {
	_, err := toAnthropicResponseFromResponses(&responses.Response{
		ID:     "resp_failed",
		Status: responses.ResponseStatusFailed,
		Error: responses.ResponseError{
			Code:    responses.ResponseErrorCodeServerError,
			Message: "upstream worker crashed",
		},
	})
	if err == nil {
		t.Fatal("expected failed response error")
	}
	for _, want := range []string{`responses api failed`, `id="resp_failed"`, `status="failed"`, `code="server_error"`, "upstream worker crashed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, missing %q", err.Error(), want)
		}
	}
	var failed *ResponseFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error type = %T, want ResponseFailedError", err)
	}
}

func TestToAnthropicResponsePreservesIncompleteReason(t *testing.T) {
	_, err := toAnthropicResponseFromResponses(&responses.Response{
		ID:     "resp_incomplete",
		Status: responses.ResponseStatusIncomplete,
		IncompleteDetails: responses.ResponseIncompleteDetails{
			Reason: "max_output_tokens",
		},
	})
	if err == nil {
		t.Fatal("expected incomplete response error")
	}
	for _, want := range []string{`responses api failed`, `id="resp_incomplete"`, `status="incomplete"`, `reason="max_output_tokens"`, "no output content"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, missing %q", err.Error(), want)
		}
	}
	var failed *ResponseFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error type = %T, want ResponseFailedError", err)
	}
	if failed.Retryable() {
		t.Fatalf("max_output_tokens incomplete should not be retryable: %+v", failed)
	}
}

func TestResponseFailedErrorRetryClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       ResponseFailedError
		retryable bool
		nonRetry  string
	}{
		{
			name:      "server error retries",
			err:       ResponseFailedError{Status: "failed", Code: "server_error"},
			retryable: true,
		},
		{
			name:      "unknown incomplete retries",
			err:       ResponseFailedError{Status: "incomplete"},
			retryable: true,
		},
		{
			name:     "content filter incomplete does not retry",
			err:      ResponseFailedError{Status: "incomplete", IncompleteReason: "content_filter"},
			nonRetry: "content_filter",
		},
		{
			name:     "invalid image does not retry",
			err:      ResponseFailedError{Status: "failed", Code: "invalid_image"},
			nonRetry: "invalid_image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Retryable(); got != tt.retryable {
				t.Fatalf("Retryable() = %v, want %v", got, tt.retryable)
			}
			if got := tt.err.NonRetryableReason(); got != tt.nonRetry {
				t.Fatalf("NonRetryableReason() = %q, want %q", got, tt.nonRetry)
			}
		})
	}
}

func TestSanitizeLogBodyRedactsSecrets(t *testing.T) {
	body := `{"access_token":"access-secret","refresh_token":"refresh-secret","message":"Bearer sk-testsecret"}`
	got := sanitizeLogBody(body)
	for _, secret := range []string{"access-secret", "refresh-secret", "sk-testsecret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitizeLogBody leaked %q in %q", secret, got)
		}
	}
}

func TestRequestErrorRetryAfterMSCapsHugeProviderDelay(t *testing.T) {
	err := &RequestError{retryAfter: maxRetryAfterSeconds + 3600}
	if got, want := err.RetryAfterMS(), maxRetryAfterSeconds*1000; got != want {
		t.Fatalf("RetryAfterMS() = %d, want %d", got, want)
	}
}

func TestToResponseInputItems_PreservesAssistantPhase(t *testing.T) {
	items, err := toResponseInputItems([]anthropic.Message{
		{
			Role: anthropic.RoleAssistant,
			Content: []anthropic.ContentBlock{
				{Type: "text", Text: "Still working.", Phase: "commentary"},
			},
		},
	})
	if err != nil {
		t.Fatalf("toResponseInputItems() error = %v", err)
	}
	if len(items) != 1 || items[0].OfMessage == nil {
		t.Fatalf("items = %+v, want one message item", items)
	}
	if items[0].OfMessage.Phase != responses.EasyInputMessagePhaseCommentary {
		t.Fatalf("phase = %q, want commentary", items[0].OfMessage.Phase)
	}
}

func TestToResponseInputItems_ConvertsToolResults(t *testing.T) {
	items, err := toResponseInputItems([]anthropic.Message{
		{
			Role: anthropic.RoleAssistant,
			Content: []anthropic.ContentBlock{
				anthropic.NewToolUseBlock("call_1", "Bash", []byte(`{"command":"pwd"}`)),
			},
		},
		{
			Role: anthropic.RoleUser,
			Content: []anthropic.ContentBlock{
				anthropic.NewToolResultBlock("call_1", "/repo", false),
			},
		},
	})
	if err != nil {
		t.Fatalf("toResponseInputItems() error = %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}

	if items[0].OfFunctionCall == nil || items[0].OfFunctionCall.CallID != "call_1" {
		t.Fatalf("first item = %+v, want function_call with call_1", items[0])
	}
	if items[1].OfFunctionCallOutput == nil || items[1].OfFunctionCallOutput.CallID != "call_1" {
		t.Fatalf("second item = %+v, want function_call_output with call_1", items[1])
	}
}

func TestToResponseInputItems_SkipsOrphanToolResults(t *testing.T) {
	items, err := toResponseInputItems([]anthropic.Message{
		{
			Role: anthropic.RoleUser,
			Content: []anthropic.ContentBlock{
				anthropic.NewToolResultBlock("call_missing", "/repo", false),
			},
		},
		{
			Role: anthropic.RoleUser,
			Content: []anthropic.ContentBlock{
				anthropic.NewTextBlock("continue"),
			},
		},
	})
	if err != nil {
		t.Fatalf("toResponseInputItems() error = %v", err)
	}
	if len(items) != 1 || items[0].OfMessage == nil {
		t.Fatalf("items = %+v, want only the text message", items)
	}
}

func TestToResponseInputItems_ConvertsCompaction(t *testing.T) {
	items, err := toResponseInputItems([]anthropic.Message{
		{
			Role: anthropic.RoleAssistant,
			Content: []anthropic.ContentBlock{
				anthropic.NewCompactionBlock("cmp_1", "encrypted-payload", "openai"),
			},
		},
	})
	if err != nil {
		t.Fatalf("toResponseInputItems() error = %v", err)
	}
	if len(items) != 1 || items[0].OfCompaction == nil {
		t.Fatalf("items = %+v, want one compaction item", items)
	}
	if items[0].OfCompaction.EncryptedContent != "encrypted-payload" {
		t.Fatalf("encrypted content = %q, want encrypted-payload", items[0].OfCompaction.EncryptedContent)
	}
	if items[0].OfCompaction.ID.Value != "cmp_1" {
		t.Fatalf("compaction id = %q, want cmp_1", items[0].OfCompaction.ID.Value)
	}
}

func TestToResponseParams_IncludesContextManagement(t *testing.T) {
	params, err := toResponseParams(anthropic.CreateMessageRequest{
		Model:               "gpt-5.5",
		CompactionThreshold: 244800,
		Messages: []anthropic.Message{
			{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.NewTextBlock("hello")}},
		},
	})
	if err != nil {
		t.Fatalf("toResponseParams() error = %v", err)
	}
	if len(params.ContextManagement) != 1 {
		t.Fatalf("context management len = %d, want 1", len(params.ContextManagement))
	}
	if params.ContextManagement[0].Type != "compaction" {
		t.Fatalf("context management type = %q, want compaction", params.ContextManagement[0].Type)
	}
	if params.ContextManagement[0].CompactThreshold.Value != 244800 {
		t.Fatalf("compact threshold = %d, want 244800", params.ContextManagement[0].CompactThreshold.Value)
	}
}

func TestCompactedResponseToConversationPreservesOpaqueItem(t *testing.T) {
	raw := []byte(`{
		"id":"resp_compact",
		"object":"response.compaction",
		"created_at":1764967971,
		"output":[
			{"id":"msg_1","type":"message","status":"completed","role":"user","content":[{"type":"input_text","text":"Original task"}]},
			{"id":"cmp_1","type":"compaction","encrypted_content":"encrypted-payload","created_by":"openai"}
		],
		"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":7},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":120}
	}`)
	var resp responses.CompactedResponse
	if err := resp.UnmarshalJSON(raw); err != nil {
		t.Fatalf("unmarshal compacted response: %v", err)
	}
	got := compactedResponseToConversation(&resp)
	if got.ID != "resp_compact" {
		t.Fatalf("ID = %q, want resp_compact", got.ID)
	}
	if got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 20 || got.Usage.CacheReadInputTokens != 7 {
		t.Fatalf("usage = %+v, want input=100 output=20 cached=7", got.Usage)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Role != anthropic.RoleUser || got.Messages[0].Content[0].Text != "Original task" {
		t.Fatalf("first message = %+v, want user Original task", got.Messages[0])
	}
	block := got.Messages[1].Content[0]
	if block.Type != "compaction" || block.ID != "cmp_1" || block.EncryptedContent != "encrypted-payload" || block.CreatedBy != "openai" {
		t.Fatalf("compaction block = %+v, want cmp_1 encrypted payload", block)
	}
}

func TestToCompactParamsCodexExtras(t *testing.T) {
	params, err := toCompactParams(anthropic.CreateMessageRequest{
		Model: "gpt-5.5",
		System: []anthropic.SystemBlock{
			{Type: "text", Text: "You are a coding agent."},
		},
		Messages: []anthropic.Message{
			{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.NewTextBlock("hello")}},
		},
		Tools: []anthropic.ToolDefinition{
			{
				Name:        "Bash",
				Description: "Run a shell command",
				InputSchema: json.RawMessage(`{"properties":{"cmd":{"type":"string"}}}`),
			},
		},
		Thinking:      &anthropic.ThinkingConfig{Type: "enabled", BudgetTokens: 16000},
		TextVerbosity: "low",
	}, true)
	if err != nil {
		t.Fatalf("toCompactParams() error = %v", err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["instructions"] != "You are a coding agent." {
		t.Fatalf("instructions = %v, want system prompt", body["instructions"])
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one tool", body["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v, want object", tools[0])
	}
	if tool["type"] != "function" || tool["name"] != "Bash" || tool["strict"] != false {
		t.Fatalf("tool = %#v, want function Bash strict=false", tool)
	}
	toolParams, ok := tool["parameters"].(map[string]any)
	if !ok || toolParams["type"] != "object" || toolParams["properties"] == nil {
		t.Fatalf("tool parameters = %#v, want object schema with properties", tool["parameters"])
	}
	if body["parallel_tool_calls"] != true {
		t.Fatalf("parallel_tool_calls = %v, want true", body["parallel_tool_calls"])
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning = %#v, want effort=xhigh", body["reasoning"])
	}
	text, ok := body["text"].(map[string]any)
	if !ok || text["verbosity"] != "low" {
		t.Fatalf("text = %#v, want verbosity=low", body["text"])
	}

	params, err = toCompactParams(anthropic.CreateMessageRequest{Model: "gpt-5.5"}, true)
	if err != nil {
		t.Fatalf("toCompactParams(empty) error = %v", err)
	}
	raw, err = json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal empty params: %v", err)
	}
	body = map[string]any{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal empty body: %v", err)
	}
	if _, ok := body["instructions"]; ok {
		t.Fatalf("instructions present for empty Codex compact body: %#v", body["instructions"])
	}
	tools, ok = body["tools"].([]any)
	if !ok || len(tools) != 0 {
		t.Fatalf("empty tools = %#v, want []", body["tools"])
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("empty parallel_tool_calls = %v, want false", body["parallel_tool_calls"])
	}
}

func TestCompactConversationUsesCodexBackendJSON(t *testing.T) {
	session, err := NewOAuthAuthSessionFromSecretData([]byte(`{
		"tokens":{"access_token":"oauth-access","refresh_token":"oauth-refresh","account_id":"acct-1"},
		"last_refresh":"2099-01-01T00:00:00Z"
	}`), "")
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromSecretData() error = %v", err)
	}

	var body map[string]any
	client := &Client{
		authSession: session,
		baseURL:     "https://chatgpt.com/backend-api/codex",
		apiMode:     apiModeResponses,
		httpClient: &http.Client{Transport: &authRoundTripper{
			session: session,
			base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if got := req.URL.String(); !strings.Contains(got, "/responses/compact?client_version="+DefaultCodexClientVersion) {
					t.Fatalf("url = %q, want compact endpoint with client_version", got)
				}
				if got := req.Header.Get("Authorization"); got != "Bearer oauth-access" {
					t.Fatalf("Authorization = %q, want OAuth bearer", got)
				}
				if got := req.Header.Get("Accept"); got != "application/json" {
					t.Fatalf("Accept = %q, want application/json", got)
				}
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatalf("unmarshal request body: %v; raw=%s", err, string(raw))
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{
						"id":"resp_compact",
						"object":"response.compaction",
						"output":[
							{"id":"msg_1","type":"message","status":"completed","role":"user","content":[{"type":"input_text","text":"Original task"}]},
							{"id":"cmp_1","type":"compaction","encrypted_content":"encrypted-payload","created_by":"openai"}
						],
						"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":7},"output_tokens":20,"total_tokens":120}
					}`)),
					Request: req,
				}, nil
			}),
		}},
		sem: make(chan struct{}, 1),
	}

	got, err := client.CompactConversation(context.Background(), anthropic.CreateMessageRequest{
		Model: "gpt-5.5",
		Messages: []anthropic.Message{
			{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.NewTextBlock("Original task")}},
		},
		Tools: []anthropic.ToolDefinition{
			{
				Name:        "Bash",
				Description: "Run command",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
		Thinking:      &anthropic.ThinkingConfig{Type: "enabled", BudgetTokens: 1},
		TextVerbosity: "medium",
	})
	if err != nil {
		t.Fatalf("CompactConversation() error = %v", err)
	}
	if body["tools"] == nil || body["parallel_tool_calls"] != true {
		t.Fatalf("compact request body missing Codex tool fields: %#v", body)
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one tool", body["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v, want object", tools[0])
	}
	toolParams, ok := tool["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("tool parameters = %#v, want object", tool["parameters"])
	}
	props, ok := toolParams["properties"].(map[string]any)
	if !ok || len(props) != 0 {
		t.Fatalf("tool properties = %#v, want empty object", toolParams["properties"])
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "minimal" {
		t.Fatalf("reasoning = %#v, want minimal", body["reasoning"])
	}
	text, ok := body["text"].(map[string]any)
	if !ok || text["verbosity"] != "medium" {
		t.Fatalf("text = %#v, want medium", body["text"])
	}
	if got.ID != "resp_compact" || len(got.Messages) != 2 {
		t.Fatalf("compacted response = %+v, want response with two messages", got)
	}
	if block := got.Messages[1].Content[0]; block.Type != "compaction" || block.EncryptedContent != "encrypted-payload" {
		t.Fatalf("compaction block = %+v, want encrypted payload", block)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: "https://api.openai.com/v1"},
		{in: "https://api.openai.com/v1", want: "https://api.openai.com/v1"},
		{in: "https://api.openai.com/v1/chat/completions", want: "https://api.openai.com/v1"},
		{in: "https://api.openai.com/v1/responses", want: "https://api.openai.com/v1"},
		{in: "https://example.com", want: "https://example.com/v1"},
		{in: "https://chatgpt.com/backend-api/codex", want: "https://chatgpt.com/backend-api/codex"},
		{in: "https://chatgpt.com/backend-api/codex/responses", want: "https://chatgpt.com/backend-api/codex"},
		{in: "https://api.githubcopilot.com", want: "https://api.githubcopilot.com"},
		{in: "https://api.githubcopilot.com/chat/completions", want: "https://api.githubcopilot.com"},
	}

	for _, tt := range tests {
		got := normalizeBaseURL(tt.in)
		if got != tt.want {
			t.Fatalf("normalizeBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShouldUseCodexBackendResponses(t *testing.T) {
	if !(&Client{authSession: &OpenAIAuthSession{mode: AuthModeOAuth}, baseURL: "https://chatgpt.com/backend-api/codex"}).shouldUseCodexBackendResponses() {
		t.Fatal("expected OAuth ChatGPT backend to use Codex response collection")
	}
	if (&Client{authSession: &OpenAIAuthSession{mode: AuthModeAPIKey}, baseURL: "https://chatgpt.com/backend-api/codex"}).shouldUseCodexBackendResponses() {
		t.Fatal("api-key ChatGPT backend should not use Codex response collection")
	}
	if (&Client{authSession: &OpenAIAuthSession{mode: AuthModeOAuth}, baseURL: "https://api.openai.com/v1"}).shouldUseCodexBackendResponses() {
		t.Fatal("standard API should not use Codex response collection")
	}
}

func TestNormalizeCompletionsURL(t *testing.T) {
	tests := []struct {
		name       string
		rawBaseURL string
		baseURL    string
		want       string
	}{
		{
			name:       "chat endpoint passthrough",
			rawBaseURL: "https://example.test/v1/chat/completions",
			baseURL:    "https://example.test/v1",
			want:       "https://example.test/v1/chat/completions",
		},
		{
			name:       "base url appends chat path",
			rawBaseURL: "https://example.test/v1",
			baseURL:    "https://example.test/v1",
			want:       "https://example.test/v1/chat/completions",
		},
		{
			name:       "copilot base appends unversioned chat path",
			rawBaseURL: "https://api.githubcopilot.com",
			baseURL:    "https://api.githubcopilot.com",
			want:       "https://api.githubcopilot.com/chat/completions",
		},
	}

	for _, tt := range tests {
		got := normalizeCompletionsURL(tt.rawBaseURL, tt.baseURL)
		if got != tt.want {
			t.Fatalf("%s: normalizeCompletionsURL(%q, %q) = %q, want %q", tt.name, tt.rawBaseURL, tt.baseURL, got, tt.want)
		}
	}
}

func TestShouldFallbackToChatCompletions(t *testing.T) {
	if !shouldFallbackToChatCompletions(&RequestError{StatusCode: 404, Body: "not found"}) {
		t.Fatalf("expected fallback for 404")
	}
	if !shouldFallbackToChatCompletions(&RequestError{StatusCode: 400, Body: "responses endpoint is unsupported"}) {
		t.Fatalf("expected fallback for responses unsupported 400")
	}
	if shouldFallbackToChatCompletions(&RequestError{StatusCode: 400, Body: "model not found"}) {
		t.Fatalf("unexpected fallback for model-not-found")
	}
}

func TestShouldFallbackToResponses(t *testing.T) {
	if !shouldFallbackToResponses(&RequestError{StatusCode: 404, Body: "not found"}) {
		t.Fatalf("expected fallback for 404")
	}
	if !shouldFallbackToResponses(&RequestError{StatusCode: 400, Body: "chat completions unsupported"}) {
		t.Fatalf("expected fallback for chat/completions unsupported 400")
	}
	if shouldFallbackToResponses(&RequestError{StatusCode: 400, Body: "model not found"}) {
		t.Fatalf("unexpected fallback for model-not-found")
	}
}

func TestShouldUseResponsesFirst(t *testing.T) {
	c := NewClient("test-key", WithBaseURL("https://example.test/v1/chat/completions"))
	if !c.shouldUseResponsesFirst("gpt-4.1") {
		t.Fatalf("expected default mode to use responses-first flow")
	}
}

func TestShouldUseResponsesFirstHonorsForcedAPIMode(t *testing.T) {
	cResponses := NewClient("test-key", WithAPIMode("responses"))
	if !cResponses.shouldUseResponsesFirst("gpt-4.1") {
		t.Fatalf("expected forced responses mode to use responses first")
	}

	cChat := NewClient("test-key", WithAPIMode("chat-completions"))
	if cChat.shouldUseResponsesFirst("gpt-5.3-codex") {
		t.Fatalf("expected forced chat mode to use chat-completions first")
	}
}

func TestNormalizeAPIMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "", want: "responses"},
		{input: "auto", want: "responses"},
		{input: "responses", want: "responses"},
		{input: "chat-completions", want: "chat-completions"},
		{input: "CHAT-COMPLETIONS", want: "chat-completions"},
		{input: "unknown", want: "responses"},
	}
	for _, tt := range tests {
		if got := normalizeAPIMode(tt.input); got != tt.want {
			t.Fatalf("normalizeAPIMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestOpenRouterDefaultUsesChatCompletionsFirst(t *testing.T) {
	c := NewClient("test-key", WithBaseURL("https://openrouter.ai/api/v1/chat/completions"))
	if c.completionsURL != "https://openrouter.ai/api/v1/chat/completions" {
		t.Fatalf("completionsURL = %q, want %q", c.completionsURL, "https://openrouter.ai/api/v1/chat/completions")
	}
}

func TestToAnthropicResponse_HandlesReasoningOutput(t *testing.T) {
	resp := &responses.Response{
		ID:    "resp_reasoning",
		Model: "gpt-5.4",
		Output: []responses.ResponseOutputItemUnion{
			{
				ID:               "rs_1",
				Type:             "reasoning",
				EncryptedContent: "encrypted_reasoning",
				Content: []responses.ResponseOutputMessageContentUnion{
					{Type: "output_text", Text: "Let me think about this..."},
				},
			},
			{
				Type: "message",
				Content: []responses.ResponseOutputMessageContentUnion{
					{Type: "output_text", Text: "The answer is 42."},
				},
			},
		},
		Usage: responses.ResponseUsage{InputTokens: 10, OutputTokens: 20},
	}

	got, err := toAnthropicResponseFromResponses(resp)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	if len(got.Content) != 2 {
		t.Fatalf("Content len = %d, want 2", len(got.Content))
	}
	if got.Content[0].Type != "thinking" {
		t.Fatalf("first block type = %q, want thinking", got.Content[0].Type)
	}
	if got.Content[0].Thinking != "Let me think about this..." {
		t.Fatalf("thinking text = %q", got.Content[0].Thinking)
	}
	if got.Content[0].ID != "rs_1" || got.Content[0].EncryptedContent != "encrypted_reasoning" {
		t.Fatalf("thinking metadata = id:%q encrypted:%q", got.Content[0].ID, got.Content[0].EncryptedContent)
	}
	if got.Content[1].Type != "text" {
		t.Fatalf("second block type = %q, want text", got.Content[1].Type)
	}
}

func TestToResponseInputItemsIncludesEncryptedReasoning(t *testing.T) {
	items, err := toResponseInputItems([]anthropic.Message{
		{
			Role: anthropic.RoleAssistant,
			Content: []anthropic.ContentBlock{
				{Type: "thinking", ID: "rs_1", EncryptedContent: "encrypted_reasoning"},
			},
		},
	})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	got := items[0].GetEncryptedContent()
	if got == nil || *got != "encrypted_reasoning" {
		t.Fatalf("encrypted reasoning = %v", got)
	}
}

func TestToAnthropicResponse_HandlesBuiltInToolCalls(t *testing.T) {
	resp := &responses.Response{
		ID:    "resp_tools",
		Model: "gpt-5.4",
		Output: []responses.ResponseOutputItemUnion{
			{
				Type:      "web_search_call",
				CallID:    "call_ws1",
				Name:      "web_search",
				Arguments: responses.ResponseOutputItemUnionArguments{OfString: `{"query":"golang"}`},
			},
		},
		Usage: responses.ResponseUsage{InputTokens: 5, OutputTokens: 10},
	}

	got, err := toAnthropicResponseFromResponses(resp)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	if len(got.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(got.Content))
	}
	if got.Content[0].Type != "tool_use" {
		t.Fatalf("block type = %q, want tool_use", got.Content[0].Type)
	}
	if got.Content[0].ID != "call_ws1" {
		t.Fatalf("call id = %q, want call_ws1", got.Content[0].ID)
	}
	if got.StopReason != anthropic.StopReasonToolUse {
		t.Fatalf("StopReason = %q, want tool_use", got.StopReason)
	}
}

func TestToResponseParams_SetsTruncationAndCacheRetention(t *testing.T) {
	req := anthropic.CreateMessageRequest{
		Model:     "gpt-5.4",
		MaxTokens: 4096,
		Messages:  []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "hi"}}}},
	}

	params, err := toResponseParams(req)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	if string(params.Truncation) != "auto" {
		t.Fatalf("Truncation = %q, want auto", params.Truncation)
	}
	if string(params.PromptCacheRetention) != "24h" {
		t.Fatalf("PromptCacheRetention = %q, want 24h", params.PromptCacheRetention)
	}
	if len(params.Include) != 0 {
		t.Fatalf("Include len = %d, want 0 (no reasoning)", len(params.Include))
	}
}

func TestImageAnalysisResponseParamsUsesResponsesImageInput(t *testing.T) {
	params, err := imageAnalysisResponseParams("gpt-5.5", "data:image/png;base64,aGVsbG8=", "describe it", "high")
	if err != nil {
		t.Fatalf("imageAnalysisResponseParams() error = %v", err)
	}

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	if body["model"] != "gpt-5.5" {
		t.Fatalf("model = %v, want gpt-5.5", body["model"])
	}
	input := body["input"].([]any)
	message := input[0].(map[string]any)
	content := message["content"].([]any)
	if content[0].(map[string]any)["type"] != "input_text" || content[0].(map[string]any)["text"] != "describe it" {
		t.Fatalf("text content = %#v", content[0])
	}
	image := content[1].(map[string]any)
	if image["type"] != "input_image" || image["detail"] != "high" || image["image_url"] != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image content = %#v", image)
	}
}

func TestAnalyzeImagePostsGPT55ImageInput(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %q, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("Authorization = %q, want bearer API key", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if err := json.Unmarshal(raw, &captured); err != nil {
			t.Fatalf("unmarshal request: %v\n%s", err, raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"type":"response.created","response":{"id":"resp_vision","model":"gpt-5.5","usage":{"input_tokens":1}}}`,
			`{"type":"response.content_part.added","output_index":0,"part":{"type":"output_text"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"vision ok"}`,
			`{"type":"response.output_text.done","output_index":0}`,
			`{"type":"response.completed","response":{"id":"resp_vision","model":"gpt-5.5","usage":{"input_tokens":2,"output_tokens":3},"output":[]}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewClient("sk-test", WithBaseURL(server.URL+"/v1"))
	resp, err := client.AnalyzeImage(context.Background(), "gpt-5.5", "data:image/png;base64,aGVsbG8=", "what is shown?", "low")
	if err != nil {
		t.Fatalf("AnalyzeImage() error = %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "vision ok" {
		t.Fatalf("response content = %+v", resp.Content)
	}

	if captured["model"] != "gpt-5.5" {
		t.Fatalf("captured model = %v", captured["model"])
	}
	input := captured["input"].([]any)
	message := input[0].(map[string]any)
	content := message["content"].([]any)
	image := content[1].(map[string]any)
	if image["type"] != "input_image" || image["detail"] != "low" || image["image_url"] != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("captured image content = %#v", image)
	}
}

func TestToResponseParams_IncludesReasoningWhenThinkingEnabled(t *testing.T) {
	req := anthropic.CreateMessageRequest{
		Model:         "gpt-5.4",
		MaxTokens:     4096,
		Messages:      []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "hi"}}}},
		Thinking:      &anthropic.ThinkingConfig{BudgetTokens: 16000},
		TextVerbosity: "low",
	}

	params, err := toResponseParams(req)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	found := false
	for _, inc := range params.Include {
		if string(inc) == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Include should contain reasoning.encrypted_content, got %v", params.Include)
	}
	if string(params.Reasoning.Effort) != "xhigh" {
		t.Fatalf("Reasoning effort = %q, want xhigh", params.Reasoning.Effort)
	}
	if params.Text.Verbosity != responses.ResponseTextConfigVerbosityLow {
		t.Fatalf("Text verbosity = %q, want low", params.Text.Verbosity)
	}
}

func TestResponsesStreamReader_TranslatesTextEvents(t *testing.T) {
	reader := &ResponsesStreamReader{}

	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:     "response.created",
		Response: responses.Response{ID: "resp_1", Model: "gpt-5.4", Usage: responses.ResponseUsage{InputTokens: 10}},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.content_part.added",
		OutputIndex: 0,
		Part:        responses.ResponseStreamEventUnionPart{Type: "output_text"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_text.delta",
		OutputIndex: 0,
		Delta:       "Hello ",
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_text.delta",
		OutputIndex: 0,
		Delta:       "world!",
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_text.done",
		OutputIndex: 0,
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:     "response.completed",
		Response: responses.Response{ID: "resp_1", Model: "gpt-5.4", Usage: responses.ResponseUsage{InputTokens: 10, OutputTokens: 5}},
	})

	events := reader.buf
	if len(events) < 7 {
		t.Fatalf("expected at least 7 events, got %d", len(events))
	}

	if events[0].Type != anthropic.EventMessageStart {
		t.Fatalf("event[0].Type = %q, want message_start", events[0].Type)
	}
	if events[1].Type != anthropic.EventContentBlockStart || events[1].ContentBlock.Type != "text" {
		t.Fatalf("event[1] = %+v, want content_block_start/text", events[1])
	}
	if events[2].Delta.Text != "Hello " {
		t.Fatalf("event[2] delta text = %q, want 'Hello '", events[2].Delta.Text)
	}
	if events[3].Delta.Text != "world!" {
		t.Fatalf("event[3] delta text = %q, want 'world!'", events[3].Delta.Text)
	}
	if events[4].Type != anthropic.EventContentBlockStop {
		t.Fatalf("event[4].Type = %q, want content_block_stop", events[4].Type)
	}
	if events[5].Type != anthropic.EventMessageDelta || events[5].Delta.StopReason != string(anthropic.StopReasonEndTurn) {
		t.Fatalf("event[5] = %+v, want message_delta/end_turn", events[5])
	}
	if events[5].Usage == nil || events[5].Usage.InputTokens != 10 || events[5].Usage.OutputTokens != 5 {
		t.Fatalf("event[5].Usage = %+v, want input=10 output=5", events[5].Usage)
	}
	if events[6].Type != anthropic.EventMessageStop {
		t.Fatalf("event[6].Type = %q, want message_stop", events[6].Type)
	}
}

func TestResponsesStreamReader_PreservesMessagePhase(t *testing.T) {
	reader := &ResponsesStreamReader{}

	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item: responses.ResponseOutputItemUnion{
			Type:  "message",
			Phase: responses.ResponseOutputMessagePhaseCommentary,
		},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.content_part.added",
		OutputIndex: 0,
		Part:        responses.ResponseStreamEventUnionPart{Type: "output_text"},
	})

	events := reader.buf
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if events[0].ContentBlock == nil || events[0].ContentBlock.Phase != "commentary" {
		t.Fatalf("content block = %+v, want commentary phase", events[0].ContentBlock)
	}
}

// TestResponsesStreamReader_TranslatesReasoningSummary verifies that gpt-5 /
// codex reasoning summaries (response.reasoning_summary_text.*) surface as a
// single thinking block, closed exactly once by output_item.done.
func TestResponsesStreamReader_TranslatesReasoningSummary(t *testing.T) {
	reader := &ResponsesStreamReader{}

	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:     "response.created",
		Response: responses.Response{ID: "resp_1", Model: "gpt-5.4"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item:        responses.ResponseOutputItemUnion{Type: "reasoning", ID: "rs_1"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.reasoning_summary_text.delta",
		OutputIndex: 0,
		Delta:       "Analyzing ",
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.reasoning_summary_text.delta",
		OutputIndex: 0,
		Delta:       "the request.",
	})
	// Per-part done with full text: ignored because deltas were already emitted.
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.reasoning_summary_text.done",
		OutputIndex: 0,
		Text:        "Analyzing the request.",
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item:        responses.ResponseOutputItemUnion{Type: "reasoning", ID: "rs_1"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:     "response.completed",
		Response: responses.Response{ID: "resp_1", Model: "gpt-5.4", Usage: responses.ResponseUsage{InputTokens: 3, OutputTokens: 7}},
	})

	assembler := anthropic.NewStreamAssembler()
	for _, ev := range reader.buf {
		assembler.Add(ev)
	}
	resp := assembler.Response()

	var thinking []anthropic.ContentBlock
	for _, b := range resp.Content {
		if b.Type == "thinking" {
			thinking = append(thinking, b)
		}
	}
	if len(thinking) != 1 {
		t.Fatalf("thinking blocks = %d, want exactly 1 (no double-close): %+v", len(thinking), resp.Content)
	}
	if thinking[0].Thinking != "Analyzing the request." {
		t.Fatalf("summary text = %q, want 'Analyzing the request.'", thinking[0].Thinking)
	}
}

func TestResponsesStreamReader_IgnoresContainerDoneEvents(t *testing.T) {
	reader := &ResponsesStreamReader{}

	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:     "response.created",
		Response: responses.Response{ID: "resp_1", Model: "gpt-5.4"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.content_part.added",
		OutputIndex: 0,
		Part:        responses.ResponseStreamEventUnionPart{Type: "output_text"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_text.delta",
		OutputIndex: 0,
		Delta:       "Hello",
	})
	// Leaf-level done → should emit content_block_stop.
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_text.done",
		OutputIndex: 0,
	})
	// Container-level done events → should be ignored (no extra block_stop).
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.content_part.done",
		OutputIndex: 0,
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_item.done",
		OutputIndex: 0,
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:     "response.completed",
		Response: responses.Response{ID: "resp_1", Model: "gpt-5.4", Usage: responses.ResponseUsage{OutputTokens: 2}},
	})

	stopCount := 0
	for _, ev := range reader.buf {
		if ev.Type == anthropic.EventContentBlockStop {
			stopCount++
		}
	}
	if stopCount != 1 {
		t.Errorf("expected exactly 1 content_block_stop, got %d (container done events should be ignored)", stopCount)
	}
}

func TestResponsesStreamReader_TranslatesToolCallEvents(t *testing.T) {
	reader := &ResponsesStreamReader{}

	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:     "response.in_progress",
		Response: responses.Response{ID: "resp_2", Model: "gpt-5.4"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item:        responses.ResponseOutputItemUnion{Type: "function_call", CallID: "call_abc", Name: "Bash"},
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 0,
		Delta:       `{"command":`,
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 0,
		Delta:       `"ls"}`,
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 0,
	})
	reader.translateEvent(responses.ResponseStreamEventUnion{
		Type: "response.completed",
		Response: responses.Response{
			ID:    "resp_2",
			Model: "gpt-5.4",
			Output: []responses.ResponseOutputItemUnion{
				{Type: "function_call", CallID: "call_abc", Name: "Bash"},
			},
			Usage: responses.ResponseUsage{InputTokens: 10, OutputTokens: 5},
		},
	})

	events := reader.buf
	if events[0].Type != anthropic.EventMessageStart {
		t.Fatalf("event[0] = %q, want message_start", events[0].Type)
	}
	if events[1].Type != anthropic.EventContentBlockStart || events[1].ContentBlock.Type != "tool_use" {
		t.Fatalf("event[1] = %+v, want content_block_start/tool_use", events[1])
	}
	if events[1].ContentBlock.Name != "Bash" {
		t.Fatalf("tool name = %q, want Bash", events[1].ContentBlock.Name)
	}
	if events[2].Delta.Type != "input_json_delta" || events[2].Delta.PartialJSON != `{"command":` {
		t.Fatalf("event[2] delta = %+v", events[2].Delta)
	}

	// Last events: content_block_stop, message_delta, message_stop
	msgDelta := events[len(events)-2]
	if msgDelta.Delta.StopReason != string(anthropic.StopReasonToolUse) {
		t.Fatalf("stop_reason = %q, want tool_use", msgDelta.Delta.StopReason)
	}
	if msgDelta.Usage == nil || msgDelta.Usage.InputTokens != 10 || msgDelta.Usage.OutputTokens != 5 {
		t.Fatalf("msgDelta.Usage = %+v, want input=10 output=5", msgDelta.Usage)
	}
}

func TestDrainStreamRejectsEmptyResponse(t *testing.T) {
	_, err := drainStreamToResponse(&StreamReader{events: []anthropic.StreamEvent{
		{
			Type: anthropic.EventMessageStart,
			Message: &anthropic.CreateMessageResponse{
				ID:    "resp_empty",
				Model: "gpt-5.5",
				Role:  anthropic.RoleAssistant,
			},
		},
		{Type: anthropic.EventMessageStop},
	}})
	if err == nil || !strings.Contains(err.Error(), "without output content") {
		t.Fatalf("error = %v, want without output content", err)
	}
}

func TestDrainStreamPreservesInputTokensWhenDeltaOmitsThem(t *testing.T) {
	resp, err := drainStreamToResponse(&StreamReader{events: []anthropic.StreamEvent{
		{
			Type: anthropic.EventMessageStart,
			Message: &anthropic.CreateMessageResponse{
				ID:    "resp_usage",
				Model: "gpt-5.5",
				Role:  anthropic.RoleAssistant,
				Usage: anthropic.Usage{InputTokens: 123},
			},
		},
		{
			Type:  anthropic.EventContentBlockStart,
			Index: 0,
			ContentBlock: &anthropic.ContentBlock{
				Type: "text",
			},
		},
		{
			Type:  anthropic.EventContentBlockDelta,
			Index: 0,
			Delta: &anthropic.DeltaBlock{
				Type: "text_delta",
				Text: "ok",
			},
		},
		{
			Type:  anthropic.EventContentBlockStop,
			Index: 0,
		},
		{
			Type: anthropic.EventMessageDelta,
			Delta: &anthropic.DeltaBlock{
				Type:       "message_delta",
				StopReason: string(anthropic.StopReasonEndTurn),
			},
			Usage: &anthropic.Usage{OutputTokens: 7},
		},
		{Type: anthropic.EventMessageStop},
	}})
	if err != nil {
		t.Fatalf("drainStreamToResponse() error = %v", err)
	}
	if resp.Usage.InputTokens != 123 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v, want input=123 output=7", resp.Usage)
	}
}
