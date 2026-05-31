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
