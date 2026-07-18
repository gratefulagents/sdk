//go:build linux

package tracestore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestFilesystemTraceStoreAppendsInPlace verifies that AppendTrace extends the
// existing file (O_APPEND) instead of rewriting it, so appending N records
// costs O(total bytes) rather than O(N x accumulated bytes).
func TestFilesystemTraceStoreAppendsInPlace(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	createTestRun(t, store, "run-1")

	path := filepath.Join(root, "traces", "run-1", "events.jsonl")
	if err := store.AppendTrace("run-1", "events", []byte(`{"n":0}`)); err != nil {
		t.Fatal(err)
	}
	firstInode := inodeOf(t, path)
	for i := 1; i < 50; i++ {
		if err := store.AppendTrace("run-1", "events", []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := inodeOf(t, path); got != firstInode {
		t.Fatalf("append rewrote the file: inode %d -> %d", firstInode, got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 50 {
		t.Fatalf("lines = %d, want 50", len(lines))
	}
}

// TestFilesystemTraceStoreRotatesAtChunkLimit verifies chunk rotation and the
// bounded ErrTraceCategoryFull failure once all rotation slots are used.
func TestFilesystemTraceStoreRotatesAtChunkLimit(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	store.maxAppendFileBytes = 32
	store.maxRotations = 2
	createTestRun(t, store, "run-1")

	record := []byte(`{"payload":"0123456789"}`) // 25 bytes + newline

	// Each chunk fits exactly one record; two rotation slots allow three
	// chunks in total.
	for i := 0; i < 3; i++ {
		if err := store.AppendTrace("run-1", "events", record); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := store.AppendTrace("run-1", "events", record); !errors.Is(err, ErrTraceCategoryFull) {
		t.Fatalf("append after quota error = %v, want ErrTraceCategoryFull", err)
	}
	for _, name := range []string{"events.jsonl", "events.jsonl.001", "events.jsonl.002"} {
		data, err := os.ReadFile(filepath.Join(root, "traces", "run-1", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(record)+"\n" {
			t.Fatalf("%s = %q, want one record", name, data)
		}
	}
}

func TestFilesystemTraceStoreRejectsOversizedEventAndFile(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	store.maxEventBytes = 16
	store.maxWriteFileBytes = 16
	createTestRun(t, store, "run-1")

	if err := store.AppendTrace("run-1", "events", []byte(strings.Repeat("x", 32))); !errors.Is(err, ErrTraceEventTooLarge) {
		t.Fatalf("oversized append error = %v, want ErrTraceEventTooLarge", err)
	}
	if err := store.WriteFile("run-1", "big.txt", []byte(strings.Repeat("x", 32))); !errors.Is(err, ErrTraceFileTooLarge) {
		t.Fatalf("oversized WriteFile error = %v, want ErrTraceFileTooLarge", err)
	}
}

func TestFilesystemTraceStoreConcurrentAppendsLoseNothing(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	createTestRun(t, store, "run-1")

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.AppendTrace("run-1", "events", []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(root, "traces", "run-1", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != n {
		t.Fatalf("lines = %d, want %d", len(lines), n)
	}
}
