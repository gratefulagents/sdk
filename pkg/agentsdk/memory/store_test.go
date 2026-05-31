package memory

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestVectorLiteral(t *testing.T) {
	tests := []struct {
		input []float32
		want  string
	}{
		{[]float32{}, "[]"},
		{[]float32{0.5}, "[0.5]"},
		{[]float32{0.1, 0.2, 0.3}, "[0.1,0.2,0.3]"},
		{[]float32{-1.0, 0, 1.0}, "[-1,0,1]"},
		{[]float32{0.000001}, "[1e-06]"},
	}
	for _, tt := range tests {
		if got := VectorLiteral(tt.input); got != tt.want {
			t.Fatalf("VectorLiteral(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNoopEmbedder(t *testing.T) {
	vec, err := (&NoopEmbedder{Dimension: 8}).Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(vec) != 8 {
		t.Fatalf("len(vec) = %d, want 8", len(vec))
	}
}

func TestOpenAIEmbedderDefaultHTTPClientHasTimeout(t *testing.T) {
	embedder := NewOpenAIEmbedder(nil, "", "")
	if embedder.client == nil || embedder.client.Timeout != defaultEmbeddingTimeout {
		t.Fatalf("default client timeout = %v, want %v", embedder.client.Timeout, defaultEmbeddingTimeout)
	}

	custom := &http.Client{Timeout: time.Second}
	embedder.SetHTTPClient(custom)
	if embedder.client != custom {
		t.Fatal("SetHTTPClient did not install custom client")
	}
}

func TestInMemoryStoreLifecycle(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	mem, err := store.Store(ctx, "ns", "remember the build command", []string{"build"}, "run-1", nil)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	found, err := store.Search(ctx, "ns", "build command", nil, 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(found) != 1 || found[0].ID != mem.ID {
		t.Fatalf("Search() = %#v, want stored memory", found)
	}

	listed, err := store.List(ctx, "ns", []string{"build"}, 10)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != mem.ID {
		t.Fatalf("List() = %#v, want stored memory", listed)
	}

	if err := store.Delete(ctx, "ns", mem.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	listed, err = store.List(ctx, "ns", nil, 10)
	if err != nil {
		t.Fatalf("List() after delete error = %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("List() after delete = %#v, want empty", listed)
	}
}
