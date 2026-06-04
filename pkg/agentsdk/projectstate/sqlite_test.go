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
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Kind: MemoryKindPinned, Content: "Use append-only project state.", Tags: []string{"state"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMemory(ctx, UpsertMemoryInput{Kind: MemoryKindProcedural, Content: "Run focused projectstate tests after storage changes.", TaskIDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	memories, err := store.SearchMemories(ctx, MemoryFilter{Query: "focused"})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].Kind != MemoryKindProcedural {
		t.Fatalf("memories = %+v", memories)
	}
	prime, err := store.PrimeContext(ctx, PrimeOptions{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Durable Project State", task.ID, "Use append-only project state."} {
		if !strings.Contains(prime, want) {
			t.Fatalf("prime missing %q:\n%s", want, prime)
		}
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
