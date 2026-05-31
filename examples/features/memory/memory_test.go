package memory_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/memory"
)

func TestInMemoryStoreSemanticsExample(t *testing.T) {
	ctx := context.Background()
	store := memory.NewInMemoryStore()

	first, err := store.Store(ctx, "team-a", "remember to renew SSL certs in November",
		[]string{"ops", "tls"}, "run-001", json.RawMessage(`{"kind":"reminder"}`))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if first.ID.String() == "" {
		t.Fatalf("expected non-empty UUID")
	}

	if _, err := store.Store(ctx, "team-a", "deploy guide is in /runbooks/deploy.md",
		[]string{"ops"}, "run-002", nil); err != nil {
		t.Fatalf("store2: %v", err)
	}
	if _, err := store.Store(ctx, "team-b", "do not include this in team-a results",
		[]string{"ops"}, "run-003", nil); err != nil {
		t.Fatalf("store3: %v", err)
	}

	teamA, err := store.List(ctx, "team-a", nil, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(teamA) != 2 {
		t.Fatalf("team-a list = %d, want 2 (namespace isolation broken)", len(teamA))
	}

	tagged, err := store.List(ctx, "team-a", []string{"tls"}, 10)
	if err != nil {
		t.Fatalf("list tagged: %v", err)
	}
	if len(tagged) != 1 || tagged[0].ID != first.ID {
		t.Fatalf("tag filter = %#v, want only the SSL reminder", tagged)
	}

	if err := store.Delete(ctx, "team-a", first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, _ := store.List(ctx, "team-a", nil, 10)
	if len(after) != 1 {
		t.Fatalf("after delete = %d, want 1", len(after))
	}
}

func TestNoopEmbedderIsDeterministicExample(t *testing.T) {
	emb := &memory.NoopEmbedder{}
	a, err := emb.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	b, _ := emb.Embed(context.Background(), "hello world")
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("len mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic embedding at %d: %f vs %f", i, a[i], b[i])
		}
	}
}
