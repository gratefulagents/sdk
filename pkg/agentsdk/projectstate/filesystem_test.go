package projectstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemStoreTaskLifecycleAndIndexes(t *testing.T) {
	ctx := context.Background()
	store, err := NewFilesystemStore(FilesystemOptions{
		StateDir:  filepath.Join(t.TempDir(), "state"),
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

	if _, err := os.Stat(filepath.Join(store.StateDir(), "events.jsonl")); err != nil {
		t.Fatalf("events missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.StateDir(), "indexes", "tasks.json")); err != nil {
		t.Fatalf("tasks index missing: %v", err)
	}

	reopened, err := NewFilesystemStore(FilesystemOptions{StateDir: store.StateDir(), ProjectID: "test-project"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetTask(ctx, blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocks != nil || got.DependsOn[0] != blocker.ID {
		t.Fatalf("replayed task = %+v", got)
	}
}

func TestFilesystemStoreMemoriesAndPrime(t *testing.T) {
	ctx := context.Background()
	store, err := NewFilesystemStore(FilesystemOptions{
		StateDir:  filepath.Join(t.TempDir(), "state"),
		ProjectID: "test-project",
		Actor:     "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
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

func TestProjectStateLockWithDeadPIDIsStale(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "state", "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "state.lock")
	if err := os.WriteFile(lockPath, []byte("pid=99999999\ntime=2026-05-09T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !staleProjectStateLock(lockPath) {
		t.Fatal("dead-pid lock was not considered stale")
	}
}

func TestFilesystemStoreTruncatesTornEventTailBeforeNewWrites(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	open := func() *FilesystemStore {
		store, err := NewFilesystemStore(FilesystemOptions{
			StateDir:  stateDir,
			ProjectID: "test-project",
			WorkDir:   t.TempDir(),
			Actor:     "tester",
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}

	store := open()
	if _, err := store.CreateTask(ctx, CreateTaskInput{Title: "before crash"}); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-append: a malformed, newline-less final record.
	eventsPath := filepath.Join(stateDir, eventsFileName)
	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"task_created","pay`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh store must drop the torn tail, truncate it away, and keep
	// accepting durable writes.
	store = open()
	if _, err := store.CreateTask(ctx, CreateTaskInput{Title: "after crash"}); err != nil {
		t.Fatal(err)
	}

	store = open()
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	titles := make([]string, 0, len(tasks))
	for _, task := range tasks {
		titles = append(titles, task.Title)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks after torn-tail recovery = %v, want [before crash, after crash]", titles)
	}
}
