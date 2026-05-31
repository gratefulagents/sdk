package memory_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	sdkmemory "github.com/gratefulagents/sdk/pkg/agentsdk/memory"
	sdktools "github.com/gratefulagents/sdk/pkg/agentsdk/tools"
)

type auditingStore struct {
	inner sdkmemory.Store
	mu    sync.Mutex

	stores   int
	searches int
}

func newAuditingStore() *auditingStore {
	return &auditingStore{inner: sdkmemory.NewInMemoryStore()}
}

func (s *auditingStore) Store(ctx context.Context, namespace, content string, tags []string, sourceRun string, metadata json.RawMessage) (*sdkmemory.Memory, error) {
	s.mu.Lock()
	s.stores++
	s.mu.Unlock()

	return s.inner.Store(ctx, namespace, content, tags, sourceRun, metadata)
}

func (s *auditingStore) Search(ctx context.Context, namespace, query string, tags []string, limit int) ([]sdkmemory.Memory, error) {
	s.mu.Lock()
	s.searches++
	s.mu.Unlock()

	return s.inner.Search(ctx, namespace, query, tags, limit)
}

func (s *auditingStore) List(ctx context.Context, namespace string, tags []string, limit int) ([]sdkmemory.Memory, error) {
	return s.inner.List(ctx, namespace, tags, limit)
}

func (s *auditingStore) Delete(ctx context.Context, namespace string, id uuid.UUID) error {
	return s.inner.Delete(ctx, namespace, id)
}

func (s *auditingStore) calls() (stores int, searches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stores, s.searches
}

func TestCustomStoreCanBackMemoryToolExample(t *testing.T) {
	ctx := context.Background()
	store := newAuditingStore()

	registry := sdktools.NewRegistry(
		t.TempDir(),
		sdktools.WithMemoryStore(store, "team-a", "run-001", "https://example.test/repo"),
	)
	tool := registry.Get("Memory")
	if tool == nil {
		t.Fatalf("registry missing Memory tool; names=%v", registry.Names())
	}

	stored, err := tool.Execute(ctx, json.RawMessage(`{
		"action": "store",
		"content": "deploys require a green smoke test",
		"tags": ["deploy"]
	}`), "")
	if err != nil || stored.IsError {
		t.Fatalf("store result=%+v err=%v", stored, err)
	}

	var payload sdkmemory.Memory
	if err := json.Unmarshal([]byte(stored.Content), &payload); err != nil {
		t.Fatalf("stored payload=%q err=%v", stored.Content, err)
	}
	if payload.Namespace != "team-a" || payload.SourceRun != "run-001" {
		t.Fatalf("stored memory = %+v, want namespace/source run from tool config", payload)
	}
	if !strings.Contains(string(payload.Metadata), "https://example.test/repo") {
		t.Fatalf("metadata = %s, want repo URL", payload.Metadata)
	}

	found, err := tool.Execute(ctx, json.RawMessage(`{
		"action": "search",
		"content": "smoke test",
		"tags": ["deploy"],
		"limit": 1
	}`), "")
	if err != nil || found.IsError {
		t.Fatalf("search result=%+v err=%v", found, err)
	}
	if !strings.Contains(found.Content, payload.ID.String()) {
		t.Fatalf("search output=%q, want stored memory id %s", found.Content, payload.ID)
	}

	stores, searches := store.calls()
	if stores != 1 || searches != 1 {
		t.Fatalf("custom store calls = store:%d search:%d, want 1/1", stores, searches)
	}
}
