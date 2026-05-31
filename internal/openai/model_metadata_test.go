package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseModelMetadataCodexShape(t *testing.T) {
	models, err := parseModelMetadata([]byte(`{
		"models": [{
			"slug": "gpt-5.5",
			"context_window": 272000,
			"max_context_window": 272000,
			"auto_compact_token_limit": null,
			"effective_context_window_percent": 95
		}]
	}`))
	if err != nil {
		t.Fatalf("parseModelMetadata returned error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	got := models[0]
	if got.ID != "gpt-5.5" {
		t.Fatalf("ID = %q, want gpt-5.5", got.ID)
	}
	if got.ContextWindow != 272000 || got.ResolvedContextWindow() != 272000 {
		t.Fatalf("context window = %d resolved=%d, want 272000", got.ContextWindow, got.ResolvedContextWindow())
	}
	if got.AutoCompactTokenLimit != 0 {
		t.Fatalf("auto compact limit = %d, want 0 for null", got.AutoCompactTokenLimit)
	}
	if got.EffectiveContextWindowPercent != 95 {
		t.Fatalf("effective context percent = %d, want 95", got.EffectiveContextWindowPercent)
	}
}

func TestModelMetadataEndpointAppendsCodexClientVersion(t *testing.T) {
	session := &OpenAIAuthSession{mode: AuthModeOAuth}
	got := modelMetadataEndpoint("https://chatgpt.com/backend-api/codex/responses", session)
	want := "https://chatgpt.com/backend-api/codex/models?client_version=" + DefaultCodexClientVersion
	if got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
}

func TestModelMetadataEndpointUsesCodexClientVersionOverride(t *testing.T) {
	session := &OpenAIAuthSession{
		mode: AuthModeOAuth,
		oauth: &oauthSessionState{
			clientVersion: "0.999.0",
		},
	}

	got := modelMetadataEndpoint("https://chatgpt.com/backend-api/codex", session)
	want := "https://chatgpt.com/backend-api/codex/models?client_version=0.999.0"
	if got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
}

func TestFetchModelMetadataStandardOpenAIShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q, want Bearer test-key", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-test"}]}`))
	}))
	defer server.Close()

	models, err := FetchModelMetadata(context.Background(), server.URL, NewAPIKeyAuthSession("test-key"))
	if err != nil {
		t.Fatalf("FetchModelMetadata returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-test" {
		t.Fatalf("models = %#v, want one gpt-test model", models)
	}
}
