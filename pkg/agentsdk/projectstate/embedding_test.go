package projectstate

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// fakeEmbedder maps known phrases to fixed vectors so hybrid ranking is
// deterministic without a live embedding provider. Unknown text gets a
// stable hashed vector that is dissimilar to the seeded ones.
type fakeEmbedder struct {
	model   string
	vectors map[string][]float32
	calls   int
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		f.calls++
		if v, ok := f.vectors[t]; ok {
			out[i] = v
			continue
		}
		// Deterministic fallback vector orthogonal-ish to the seeded ones.
		out[i] = []float32{0, 0, 1}
	}
	return out, nil
}

func TestCosineSimilarity(t *testing.T) {
	if got := cosineSimilarity([]float32{1, 0}, []float32{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("identical vectors cosine = %v, want 1", got)
	}
	if got := cosineSimilarity([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-9 {
		t.Fatalf("orthogonal vectors cosine = %v, want 0", got)
	}
	if got := cosineSimilarity([]float32{1, 0}, nil); got != 0 {
		t.Fatalf("mismatched length cosine = %v, want 0", got)
	}
}

func TestRecencyScore(t *testing.T) {
	now := time.Now()
	if got := recencyScore(now, now, time.Hour); math.Abs(got-1) > 1e-9 {
		t.Fatalf("age 0 recency = %v, want 1", got)
	}
	if got := recencyScore(now.Add(-time.Hour), now, time.Hour); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("one half-life recency = %v, want 0.5", got)
	}
}

// TestHybridSemanticRecall verifies that a query with no lexical overlap still
// retrieves the semantically nearest memory, which pure keyword search misses.
func TestHybridSemanticRecall(t *testing.T) {
	ctx := context.Background()
	emb := &fakeEmbedder{
		model: "fake-1",
		vectors: map[string][]float32{
			"The user prefers the vim editor.":    {1, 0, 0},
			"Deploys run every Friday afternoon.": {0, 1, 0},
			"What is my favorite text editor?":    {0.95, 0.1, 0},
		},
	}
	store, err := NewFilesystemStore(FilesystemOptions{
		StateDir: filepath.Join(t.TempDir(), "state"),
		Actor:    "tester",
		Embedder: emb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Kind: MemoryKindSemantic, Content: "The user prefers the vim editor."}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Kind: MemoryKindSemantic, Content: "Deploys run every Friday afternoon."}); err != nil {
		t.Fatal(err)
	}

	got, err := store.SearchMemories(ctx, MemoryFilter{Query: "What is my favorite text editor?"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("hybrid recall returned no memories")
	}
	if got[0].Content != "The user prefers the vim editor." {
		t.Fatalf("top hybrid result = %q, want the editor memory", got[0].Content)
	}
}

// TestHybridCachesEmbeddings verifies vectors are cached on write and reused on
// recall instead of being recomputed for every memory each search.
func TestHybridCachesEmbeddings(t *testing.T) {
	ctx := context.Background()
	emb := &fakeEmbedder{model: "fake-1", vectors: map[string][]float32{
		"alpha fact": {1, 0, 0},
		"beta fact":  {0, 1, 0},
	}}
	dir := filepath.Join(t.TempDir(), "state")
	store, err := NewFilesystemStore(FilesystemOptions{StateDir: dir, Embedder: emb})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Content: "alpha fact"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Content: "beta fact"}); err != nil {
		t.Fatal(err)
	}
	callsAfterWrite := emb.calls

	if _, err := store.SearchMemories(ctx, MemoryFilter{Query: "alpha fact"}); err != nil {
		t.Fatal(err)
	}
	// The two stored memories were already embedded on write, so recall should
	// only embed the query (one additional call), not re-embed the corpus.
	if delta := emb.calls - callsAfterWrite; delta != 1 {
		t.Fatalf("recall embed calls = %d, want 1 (query only)", delta)
	}
}

// TestLexicalFallbackWithoutEmbedder ensures behavior is unchanged when no
// embedder is configured.
func TestLexicalFallbackWithoutEmbedder(t *testing.T) {
	ctx := context.Background()
	store, err := NewFilesystemStore(FilesystemOptions{StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Content: "Run focused tests after storage changes."}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Content: "Use append-only project state."}); err != nil {
		t.Fatal(err)
	}
	got, err := store.SearchMemories(ctx, MemoryFilter{Query: "focused"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "Run focused tests after storage changes." {
		t.Fatalf("lexical fallback = %+v", got)
	}
	if _, err := store.SearchMemories(ctx, MemoryFilter{Query: ""}); err == nil {
		t.Fatal("empty query should error")
	}
}
