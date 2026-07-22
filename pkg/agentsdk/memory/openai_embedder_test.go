package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	openai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func TestOpenAIEmbedderRefreshesOldOpaqueOAuthToken(t *testing.T) {
	var refreshCalls atomic.Int32
	var embeddingCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh-access","refresh_token":"fresh-refresh"}`))
		case "/embeddings":
			embeddingCalls.Add(1)
			if got := req.Header.Get("Authorization"); got != "Bearer fresh-access" {
				t.Errorf("Authorization = %q, want refreshed access token", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.25,0.75]}]}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	session, err := openai.NewOAuthAuthSessionFromConfig(openai.OAuthSessionConfig{
		AuthJSON:      []byte(`{"tokens":{"access_token":"opaque-access","refresh_token":"old-refresh","account_id":"acct-1"},"last_refresh":"2000-01-01T00:00:00Z"}`),
		TokenEndpoint: server.URL + "/token",
	})
	if err != nil {
		t.Fatalf("NewOAuthAuthSessionFromConfig() error = %v", err)
	}
	embedder := NewOpenAIEmbedder(session, server.URL, "text-embedding-test")

	vector, err := embedder.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(vector) != 2 || vector[0] != 0.25 || vector[1] != 0.75 {
		t.Fatalf("embedding = %#v, want [0.25 0.75]", vector)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if got := embeddingCalls.Load(); got != 1 {
		t.Fatalf("embedding calls = %d, want 1", got)
	}
}
