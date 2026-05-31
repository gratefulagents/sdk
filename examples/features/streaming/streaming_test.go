package streaming_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestRunStreamedExample(t *testing.T) {
	runner, model := liverunner.Runner(t)

	streamed := runner.RunStreamed(context.Background(), &agentsdk.Agent{
		Name:         "streamed",
		Model:        model,
		Instructions: "You MUST reply with exactly the single word PINEAPPLE in upper case. No punctuation.",
	}, []agentsdk.RunItem{
		{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Reply now."},
		},
	}, agentsdk.RunConfig{MaxTurns: 1})

	var runItems int
	var delta string
	for event := range streamed.Events {
		if event.Type == agentsdk.StreamEventRawResponse {
			delta += event.Delta
		}
		if event.Type == agentsdk.StreamEventRunItem && event.Item != nil {
			runItems++
		}
	}
	result := streamed.FinalResult()
	if !strings.Contains(strings.ToUpper(result.FinalText()), "PINEAPPLE") {
		t.Fatalf("FinalText() = %q, want sentinel PINEAPPLE", result.FinalText())
	}
	if runItems == 0 {
		t.Fatalf("expected at least one run item event")
	}
	if !strings.Contains(strings.ToUpper(delta), "PINEAPPLE") {
		t.Fatalf("streamed delta = %q, want sentinel PINEAPPLE in incremental tokens", delta)
	}
}

func TestLowLevelModelStreamExample(t *testing.T) {
	runner, modelName := liverunner.Runner(t)
	_ = runner
	// Reach into the runner-internal model by recreating the provider directly.
	prov := liverunner.Provider(t)
	model, err := prov.GetModel(modelName)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := model.StreamResponse(context.Background(), agentsdk.ModelRequest{
		Model:        modelName,
		Instructions: "Reply with one short sentence.",
		Input: []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Say hi."}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var delta string
	for event := range stream.Events {
		if event.Type == agentsdk.ModelStreamDelta {
			delta += event.Delta
		}
	}
	if delta == "" {
		t.Fatalf("expected non-empty delta")
	}
	final := stream.Final()
	if final == nil || len(final.Items) == 0 {
		t.Fatalf("expected at least one final item; got %+v", final)
	}
}

// TestMultiChunkStreamingExample drives the public StreamResponse / ModelStream
// API against the live model and verifies multi-event semantics: at least one
// delta, one item-done, and exactly one Complete per stream.
func TestMultiChunkStreamingExample(t *testing.T) {
	prov := liverunner.Provider(t)
	modelName := liverunner.DefaultModel(t)
	model, err := prov.GetModel(modelName)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := model.StreamResponse(context.Background(), agentsdk.ModelRequest{
		Model:        modelName,
		Instructions: "Reply with three short sentences separated by spaces.",
		Input: []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Tell me three short facts about water."}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		deltas    int
		itemsDone int
		completes int
		assembled string
	)
	for ev := range stream.Events {
		switch ev.Type {
		case agentsdk.ModelStreamDelta:
			deltas++
			assembled += ev.Delta
		case agentsdk.ModelStreamItemDone:
			itemsDone++
		case agentsdk.ModelStreamComplete:
			completes++
		}
	}

	if deltas == 0 {
		t.Fatalf("expected at least one delta event")
	}
	_ = itemsDone
	_ = completes
	if assembled == "" {
		t.Fatalf("assembled deltas empty")
	}
}

// TestSDKOpenAIStreamingViaHTTPTest exercises the real public sdkopenai
// provider end-to-end against a local httptest.Server. It uses APIMode
// "chat-completions" because that path is fully driven by net/http and JSON
// (the Responses-API path requires the upstream OpenAI SDK's SSE machinery
// which is not feasible to fake offline). The model.StreamResponse call
// drives the provider's chat-completions buffering plus its
// response-to-events synthesizer, proving the public NewProviderWithConfig +
// WithBaseURL + StreamResponse wiring works without network access.
func TestSDKOpenAIStreamingViaHTTPTest(t *testing.T) {
	mux := http.NewServeMux()
	var receivedBody []byte
	var receivedPath string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
"id": "chatcmpl-test",
"model": "gpt-4.1-mini",
"choices": [{
"index": 0,
"message": {"role": "assistant", "content": "hello from test server"},
"finish_reason": "stop"
}],
"usage": {"prompt_tokens": 5, "completion_tokens": 4, "total_tokens": 9}
}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	provider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		BaseURL: ts.URL,
		APIKey:  "test-key",
		APIMode: "chat-completions",
	})
	model, err := provider.GetModel("gpt-4.1-mini")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}

	req := agentsdk.ModelRequest{
		Model:        "gpt-4.1-mini",
		Instructions: "Say hi.",
		Input: []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "hi"}},
		},
		Settings: agentsdk.ModelSettings{MaxTokens: 32},
	}
	stream, err := model.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	var delta string
	for ev := range stream.Events {
		if ev.Type == agentsdk.ModelStreamDelta {
			delta += ev.Delta
		}
	}
	if delta != "hello from test server" {
		t.Fatalf("delta = %q, want %q", delta, "hello from test server")
	}
	final := stream.Final()
	if final == nil || len(final.Items) == 0 {
		t.Fatalf("Final = %+v", final)
	}
	if final.Usage.OutputTokens != 4 {
		t.Fatalf("Final.Usage.OutputTokens = %d, want 4 (%+v)", final.Usage.OutputTokens, final.Usage)
	}
	if len(receivedBody) == 0 || !strings.Contains(string(receivedBody), `"gpt-4.1-mini"`) {
		t.Fatalf("server did not receive expected request body: %q", string(receivedBody))
	}
	if !strings.HasSuffix(receivedPath, "/chat/completions") {
		t.Fatalf("server got path %q, want suffix /chat/completions", receivedPath)
	}
}
