package projectstate

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteStoreTaskLifecycleAndReplay(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSQLiteStore(SQLiteOptions{
		Path:      dbPath,
		ProjectID: "test-project",
		WorkDir:   t.TempDir(),
		Actor:     "tester",
	})
	if err != nil {
		t.Fatal(err)
	}

	blocker, err := store.CreateTask(ctx, CreateTaskInput{Title: "Set up schema", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := store.CreateTask(ctx, CreateTaskInput{Title: "Use schema", Priority: 2, DependsOn: []string{blocker.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.ReadyTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].ID != blocker.ID {
		t.Fatalf("ready = %+v, want only blocker", ready)
	}

	if _, err := store.ClaimTask(ctx, blocker.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	ready, err = store.ReadyTasks(ctx, TaskFilter{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready while blocker in progress = %+v, want none", ready)
	}
	if _, err := store.CloseTask(ctx, blocker.ID, "done"); err != nil {
		t.Fatal(err)
	}
	ready, err = store.ReadyTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].ID != blocked.ID {
		t.Fatalf("ready = %+v, want blocked task after blocker closes", ready)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same file and confirm state replays from the events table.
	reopened, err := NewSQLiteStore(SQLiteOptions{Path: dbPath, ProjectID: "test-project"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetTask(ctx, blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocks != nil || got.DependsOn[0] != blocker.ID {
		t.Fatalf("replayed task = %+v", got)
	}
}

func TestSQLiteStoreMemoriesAndPrime(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(SQLiteOptions{
		Path:      filepath.Join(t.TempDir(), "state.db"),
		ProjectID: "test-project",
		Actor:     "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTask(ctx, CreateTaskInput{Title: "Implement durable state", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := store.SaveMemory(ctx, SaveMemoryInput{Kind: MemoryKindDecision, Title: "Use append-only project state", Body: "State is event-sourced so it can be replayed after crashes."})
	if err != nil {
		t.Fatal(err)
	}
	procedure, err := store.SaveMemory(ctx, SaveMemoryInput{Kind: MemoryKindProcedure, Title: "Test storage changes", Body: "Run focused projectstate tests after storage changes.", Citations: []Citation{{Path: "pkg/agentsdk/projectstate"}}})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchMemories(ctx, MemoryQuery{Query: "focused"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != procedure.ID || hits[0].Score <= 0 {
		t.Fatalf("hits = %+v", hits)
	}
	hits, err = store.SearchMemories(ctx, MemoryQuery{Query: "focused", Kinds: []string{MemoryKindDecision}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("kind-filtered hits = %+v, want none", hits)
	}

	if _, err := store.VerifyMemory(ctx, procedure.ID, "abc1234"); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchMemories(ctx, []string{procedure.ID, "mem_unknown"}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.SaveMemory(ctx, SaveMemoryInput{ID: procedure.ID, Kind: MemoryKindProcedure, Title: "Test storage changes", Body: "Run go test ./pkg/agentsdk/projectstate/... after storage changes."})
	if err != nil {
		t.Fatal(err)
	}
	if updated.UseCount != 1 || updated.LastUsedAt == nil || !updated.CreatedAt.Equal(procedure.CreatedAt) || updated.CommitSHA != "" {
		t.Fatalf("updated memory = %+v, want preserved usage and created_at", updated)
	}
	if _, err := store.SaveMemory(ctx, SaveMemoryInput{ID: "mem_missing", Title: "x", Body: "y"}); err == nil {
		t.Fatal("saving an unknown id should fail")
	}

	listed, err := store.ListMemories(ctx, MemoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != decision.ID || listed[1].ID != procedure.ID {
		t.Fatalf("listed = %+v, want decision before procedure", listed)
	}

	prime, err := store.PrimeContext(ctx, PrimeOptions{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Durable Project State", task.ID, "Memory Index (2 of 2)", "- " + decision.ID + " [decision] Use append-only project state"} {
		if !strings.Contains(prime, want) {
			t.Fatalf("prime missing %q:\n%s", want, prime)
		}
	}
	again, err := store.PrimeContext(ctx, PrimeOptions{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if again != prime {
		t.Fatalf("prime is not deterministic:\n%s\n---\n%s", prime, again)
	}

	if err := store.DeleteMemory(ctx, decision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetMemory(ctx, decision.ID); err == nil {
		t.Fatal("deleted memory still readable")
	}
}

// TestSQLiteStoreSharedDB verifies that a caller-provided *sql.DB is used (not
// closed) and that table prefixes keep two projects isolated in one database.
func TestSQLiteStoreSharedDB(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteFile(filepath.Join(t.TempDir(), "shared.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	a, err := NewSQLiteStore(SQLiteOptions{DB: db, ProjectID: "proj-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSQLiteStore(SQLiteOptions{DB: db, ProjectID: "proj-b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateTask(ctx, CreateTaskInput{Title: "A task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateTask(ctx, CreateTaskInput{Title: "B task"}); err != nil {
		t.Fatal(err)
	}
	aTasks, err := a.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(aTasks) != 1 || aTasks[0].Title != "A task" {
		t.Fatalf("project a tasks = %+v, want only its own", aTasks)
	}
	bTasks, err := b.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bTasks) != 1 || bTasks[0].Title != "B task" {
		t.Fatalf("project b tasks = %+v, want only its own", bTasks)
	}

	// Close on a shared-DB store must not close the underlying handle.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("shared db closed unexpectedly: %v", err)
	}
	var _ *sql.DB = b.DB()
}
