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

func TestParseModelMetadataCopilotCapabilitiesShape(t *testing.T) {
	// GitHub Copilot's /models response advertises bare IDs in the OpenAI
	// data[] shape but nests real limits under capabilities.limits.
	models, err := parseModelMetadata([]byte(`{
		"data": [{
			"id": "gpt-5.3-codex-spark",
			"capabilities": {
				"family": "gpt-5",
				"limits": {
					"max_context_window_tokens": 128000,
					"max_prompt_tokens": 96000,
					"max_output_tokens": 32000
				}
			}
		}]
	}`))
	if err != nil {
		t.Fatalf("parseModelMetadata returned error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	got := models[0]
	if got.ID != "gpt-5.3-codex-spark" {
		t.Fatalf("ID = %q, want gpt-5.3-codex-spark", got.ID)
	}
	if got.ContextWindow != 128000 || got.ResolvedContextWindow() != 128000 {
		t.Fatalf("context window = %d resolved=%d, want 128000", got.ContextWindow, got.ResolvedContextWindow())
	}
	if got.MaxOutputTokens != 32000 {
		t.Fatalf("max output tokens = %d, want 32000", got.MaxOutputTokens)
	}
}

func TestParseModelMetadataCopilotFallsBackToMaxPromptTokens(t *testing.T) {
	models, err := parseModelMetadata([]byte(`{
		"data": [{
			"id": "some-model",
			"capabilities": {"limits": {"max_prompt_tokens": 200000}}
		}]
	}`))
	if err != nil {
		t.Fatalf("parseModelMetadata returned error: %v", err)
	}
	if got := models[0]; got.ContextWindow != 200000 {
		t.Fatalf("context window = %d, want 200000 (max_prompt_tokens fallback)", got.ContextWindow)
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

func TestPickerModelMetadataMirrorsCodexPicker(t *testing.T) {
	// Shape and values mirror the ChatGPT Codex backend /models response at
	// client_version 0.153.4: Astra is priority 1 and listed, gpt-reserve and
	// codex-auto-review are hidden.
	models, err := parseModelMetadata([]byte(`{
		"models": [
			{"slug": "gpt-5.6-sol", "visibility": "list", "priority": 6, "display_name": "GPT-5.6 Sol"},
			{"slug": "gpt-reserve", "visibility": "hide", "priority": 3},
			{"slug": "gpt-6-astra", "visibility": "list", "priority": 1, "display_name": "GPT-6-Astra",
			 "description": "Our most capable model for complex, demanding work.",
			 "default_reasoning_level": "low",
			 "supported_reasoning_levels": [{"effort":"low","description":""},{"effort":"max","description":""},{"effort":"ultra","description":""}],
			 "context_window": 272000, "max_context_window": 872000, "minimal_client_version": "0.153.0"},
			{"slug": "gpt-5.4-mini", "visibility": "list", "priority": 23, "upgrade": {"model": "gpt-5.6-luna"}},
			{"slug": "codex-auto-review", "visibility": "hide", "priority": 43}
		]
	}`))
	if err != nil {
		t.Fatalf("parseModelMetadata returned error: %v", err)
	}
	picker := PickerModelMetadata(models)
	var got []string
	for _, m := range picker {
		got = append(got, m.ID)
	}
	want := []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.4-mini"}
	if len(got) != len(want) {
		t.Fatalf("picker models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picker models = %v, want %v", got, want)
		}
	}
	astra := picker[0]
	if astra.DisplayName != "GPT-6-Astra" || astra.Description == "" || astra.DefaultReasoningLevel != "low" {
		t.Fatalf("astra metadata = %+v", astra)
	}
	if len(astra.SupportedReasoningLevels) != 3 || astra.SupportedReasoningLevels[2] != "ultra" {
		t.Fatalf("astra reasoning levels = %v", astra.SupportedReasoningLevels)
	}
	if astra.ResolvedContextWindow() != 272000 {
		t.Fatalf("astra context window = %d, want 272000", astra.ResolvedContextWindow())
	}
	if picker[2].UpgradeModel != "gpt-5.6-luna" {
		t.Fatalf("gpt-5.4-mini upgrade = %q, want gpt-5.6-luna", picker[2].UpgradeModel)
	}
	for _, m := range models {
		if m.ID == "gpt-reserve" && !m.Hidden() {
			t.Fatalf("gpt-reserve should be hidden")
		}
	}
}

func TestPickerModelMetadataPlainOpenAIShapeStaysAlphabetical(t *testing.T) {
	models, err := parseModelMetadata([]byte(`{"data":[{"id":"gpt-5.5"},{"id":"gpt-4.1"},{"id":"o3"}]}`))
	if err != nil {
		t.Fatalf("parseModelMetadata returned error: %v", err)
	}
	picker := PickerModelMetadata(models)
	want := []string{"gpt-4.1", "gpt-5.5", "o3"}
	for i := range want {
		if picker[i].ID != want[i] {
			t.Fatalf("picker[%d] = %q, want %q", i, picker[i].ID, want[i])
		}
	}
}
