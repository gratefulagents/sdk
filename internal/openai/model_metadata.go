package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const modelMetadataResponseLimit = 8 << 20

// ModelMetadata is the subset of provider model metadata the SDK uses.
// The ChatGPT Codex backend returns these fields from /models.
type ModelMetadata struct {
	ID                            string
	ContextWindow                 int
	MaxContextWindow              int
	AutoCompactTokenLimit         int
	EffectiveContextWindowPercent int
}

// ResolvedContextWindow mirrors Codex's ModelInfo::resolved_context_window.
func (m ModelMetadata) ResolvedContextWindow() int {
	if m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return m.MaxContextWindow
}

// FetchModelMetadata fetches OpenAI-compatible model metadata.
//
// It supports both the standard OpenAI /v1/models shape:
//
//	{"data":[{"id":"gpt-..."}]}
//
// and the ChatGPT Codex backend shape:
//
//	{"models":[{"slug":"gpt-...","context_window":272000,...}]}
func FetchModelMetadata(ctx context.Context, baseURL string, session *OpenAIAuthSession) ([]ModelMetadata, error) {
	if session == nil {
		return nil, fmt.Errorf("auth session is required")
	}
	endpoint := modelMetadataEndpoint(baseURL, session)
	models, err := fetchModelMetadataOnce(ctx, endpoint, session)
	if err == nil {
		return models, nil
	}
	reqErr := asRequestError(err)
	if !session.SupportsRefresh() || reqErr == nil || reqErr.StatusCode != http.StatusUnauthorized {
		return nil, fmt.Errorf("querying OpenAI-compatible models from %s: %w", endpoint, err)
	}
	if refreshErr := session.Refresh(ctx); refreshErr != nil {
		return nil, fmt.Errorf("refresh OAuth token: %w", refreshErr)
	}
	models, err = fetchModelMetadataOnce(ctx, endpoint, session)
	if err != nil {
		return nil, fmt.Errorf("querying OpenAI-compatible models from %s after refresh: %w", endpoint, err)
	}
	return models, nil
}

// FetchModelMetadataByID fetches model metadata keyed by lower-case model ID.
// Prefixed aliases such as "openai/gpt-5.5" are intentionally not synthesized;
// callers can strip provider prefixes for lookup.
func FetchModelMetadataByID(ctx context.Context, baseURL string, session *OpenAIAuthSession) (map[string]ModelMetadata, error) {
	models, err := FetchModelMetadata(ctx, baseURL, session)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]ModelMetadata, len(models))
	for _, model := range models {
		if id := normalizeModelMetadataID(model.ID); id != "" {
			byID[id] = model
		}
	}
	return byID, nil
}

// IsChatGPTBackendBaseURL reports whether a base URL targets the ChatGPT backend.
func IsChatGPTBackendBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return isChatGPTBackendHost(u.Host)
}

func modelMetadataEndpoint(baseURL string, session *OpenAIAuthSession) string {
	endpoint := strings.TrimSuffix(normalizeBaseURL(baseURL), "/") + "/models"
	if session != nil && session.IsOAuth() && IsChatGPTBackendBaseURL(endpoint) {
		endpoint = appendCodexClientVersion(endpoint, session)
	}
	return endpoint
}

func appendCodexClientVersion(endpoint string, session *OpenAIAuthSession) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	q := u.Query()
	if q.Get("client_version") == "" {
		q.Set("client_version", sessionCodexClientVersion(session))
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func fetchModelMetadataOnce(ctx context.Context, endpoint string, session *OpenAIAuthSession) ([]ModelMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	headers, err := session.RequestHeaders(ctx)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := newAuthHTTPClient(nil, 20*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelMetadataResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		return nil, &RequestError{
			StatusCode: resp.StatusCode,
			Body:       snippet,
			API:        "models",
			Endpoint:   endpoint,
			retryAfter: parseRetryAfterSeconds(resp.Header),
		}
	}
	return parseModelMetadata(body)
}

func parseModelMetadata(body []byte) ([]ModelMetadata, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Slug                          string `json:"slug"`
			ContextWindow                 int    `json:"context_window"`
			MaxContextWindow              int    `json:"max_context_window"`
			AutoCompactTokenLimit         int    `json:"auto_compact_token_limit"`
			EffectiveContextWindowPercent int    `json:"effective_context_window_percent"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	models := make([]ModelMetadata, 0, len(payload.Models)+len(payload.Data))
	if len(payload.Models) > 0 {
		for _, item := range payload.Models {
			id := strings.TrimSpace(item.Slug)
			if id == "" {
				continue
			}
			models = append(models, ModelMetadata{
				ID:                            id,
				ContextWindow:                 item.ContextWindow,
				MaxContextWindow:              item.MaxContextWindow,
				AutoCompactTokenLimit:         item.AutoCompactTokenLimit,
				EffectiveContextWindowPercent: item.EffectiveContextWindowPercent,
			})
		}
	} else {
		for _, item := range payload.Data {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			models = append(models, ModelMetadata{ID: id})
		}
	}

	models = uniqueModelMetadata(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("provider returned no models")
	}
	return models, nil
}

func uniqueModelMetadata(models []ModelMetadata) []ModelMetadata {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]ModelMetadata, 0, len(models))
	for _, model := range models {
		key := normalizeModelMetadataID(model.ID)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	return out
}

func normalizeModelMetadataID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}
