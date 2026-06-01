package projectstate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAIEmbedderOptions configures an OpenAI-compatible embedding client.
//
// Any provider that implements the OpenAI /v1/embeddings contract works,
// including OpenAI itself, a local Ollama server (BaseURL
// "http://localhost:11434/v1"), and other OpenAI-compatible gateways. Point
// BaseURL at the provider you want and supply the matching model and key.
//
// Note on OpenRouter: OpenRouter focuses on chat/completions and does not
// currently expose a general-purpose /v1/embeddings endpoint. Use OpenAI or a
// local model (Ollama) for embeddings, and keep OpenRouter for completions. If
// OpenRouter adds an embeddings route, set BaseURL to it here with no code
// changes.
type OpenAIEmbedderOptions struct {
	// BaseURL is the API root, e.g. "https://api.openai.com/v1". A trailing
	// "/embeddings" is appended automatically. Defaults to the OpenAI API.
	BaseURL string
	// APIKey is sent as a Bearer token. Optional for local servers.
	APIKey string
	// ModelID is the embedding model, e.g. "text-embedding-3-small".
	ModelID string
	// Dimensions optionally requests a reduced embedding size when the model
	// supports it (e.g. OpenAI text-embedding-3-*).
	Dimensions int
	// BatchSize caps how many inputs are sent per request. Defaults to 96.
	BatchSize int
	// HTTPClient overrides the default client (30s timeout).
	HTTPClient *http.Client
	// Headers adds extra request headers (e.g. OpenRouter attribution).
	Headers map[string]string
}

// OpenAIEmbedder is an Embedder backed by an OpenAI-compatible embeddings API.
type OpenAIEmbedder struct {
	baseURL    string
	apiKey     string
	model      string
	dimensions int
	batchSize  int
	headers    map[string]string
	httpClient *http.Client
}

// NewOpenAIEmbedder builds an OpenAI-compatible embedder. It returns an error
// only when no model is provided; networking is validated lazily on first use.
func NewOpenAIEmbedder(opts OpenAIEmbedderOptions) (*OpenAIEmbedder, error) {
	model := strings.TrimSpace(opts.ModelID)
	if model == "" {
		return nil, fmt.Errorf("embedder model is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = 96
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &OpenAIEmbedder{
		baseURL:    baseURL,
		apiKey:     strings.TrimSpace(opts.APIKey),
		model:      model,
		dimensions: opts.Dimensions,
		batchSize:  batch,
		headers:    opts.Headers,
		httpClient: client,
	}, nil
}

// Model implements Embedder.
func (e *OpenAIEmbedder) Model() string { return e.model }

type embeddingsRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed implements Embedder, batching inputs to respect provider limits.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += e.batchSize {
		end := start + e.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

func (e *OpenAIEmbedder) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	body, err := json.Marshal(embeddingsRequest{Model: e.model, Input: batch, Dimensions: e.dimensions})
	if err != nil {
		return nil, fmt.Errorf("marshal embeddings request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()

	var parsed embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode embeddings response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return nil, fmt.Errorf("embeddings error (status %d): %s", resp.StatusCode, parsed.Error.Message)
		}
		return nil, fmt.Errorf("embeddings error: status %d", resp.StatusCode)
	}
	if len(parsed.Data) != len(batch) {
		return nil, fmt.Errorf("embeddings response count mismatch: got %d want %d", len(parsed.Data), len(batch))
	}
	vecs := make([][]float32, len(batch))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embeddings response index out of range: %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
