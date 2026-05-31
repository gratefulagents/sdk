package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	openai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

const (
	defaultEmbeddingTimeout  = 20 * time.Second
	maxEmbeddingResponseBody = 2 * 1024 * 1024
)

// OpenAIEmbedder embeds text through the OpenAI embeddings API.
type OpenAIEmbedder struct {
	baseURL string
	auth    *openai.OpenAIAuthSession
	model   string
	client  *http.Client
}

func NewOpenAIEmbedder(auth *openai.OpenAIAuthSession, baseURL string, model string) *OpenAIEmbedder {
	if model == "" {
		model = "text-embedding-3-small"
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAIEmbedder{
		baseURL: baseURL,
		auth:    auth,
		model:   model,
		client:  &http.Client{Timeout: defaultEmbeddingTimeout},
	}
}

type embeddingRequest struct {
	Input string `json:"input"`
	Model string `json:"model"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e.auth == nil {
		return nil, fmt.Errorf("OpenAI auth session is not configured")
	}
	body, err := json.Marshal(embeddingRequest{Input: text, Model: e.model})
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	headers, err := e.auth.RequestHeaders(ctx)
	if err != nil {
		return nil, fmt.Errorf("building auth headers: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling OpenAI embeddings API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result embeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("OpenAI API error: %s", result.Error.Message)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return result.Data[0].Embedding, nil
}

// SetHTTPClient overrides the HTTP client, mainly for tests.
func (e *OpenAIEmbedder) SetHTTPClient(client *http.Client) {
	if client != nil {
		e.client = client
	}
}
