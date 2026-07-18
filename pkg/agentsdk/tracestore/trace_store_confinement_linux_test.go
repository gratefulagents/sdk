//go:build linux

package tracestore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFilesystemTraceStoreRejectsTracesSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "traces")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystemTraceStore(root); err == nil {
		t.Fatal("NewFilesystemTraceStore() error = nil, want traces symlink rejection")
	}
	assertDirEmpty(t, outside)
}

func TestFilesystemTraceStoreRejectsRunDirSymlink(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "traces", "run-1")); err != nil {
		t.Fatal(err)
	}
	metadata := RunMetadata{RunID: "run-1", StartedAt: time.Now()}
	if _, err := store.CreateRunDir("run-1", metadata); err == nil {
		t.Fatal("CreateRunDir() error = nil, want run symlink rejection")
	}
	if err := store.AppendTrace("run-1", "events", []byte(`{}`)); err == nil {
		t.Fatal("AppendTrace() error = nil, want run symlink rejection")
	}
	if err := store.WriteFile("run-1", "artifact.txt", []byte("secret")); err == nil {
		t.Fatal("WriteFile() error = nil, want run symlink rejection")
	}
	if _, err := store.RunDir("run-1"); err == nil {
		t.Fatal("RunDir() error = nil, want run symlink rejection")
	}
	runs, err := store.ListRuns(RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("ListRuns() = %#v, want no symlinked runs", runs)
	}
	assertDirEmpty(t, outside)
}

func TestFilesystemTraceStoreRejectsNestedWriteFileSymlink(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	createTestRun(t, store, "run-1")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "traces", "run-1", "nested")); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile("run-1", "nested/artifact.txt", []byte("secret")); err == nil {
		t.Fatal("WriteFile() error = nil, want nested symlink rejection")
	}
	assertDirEmpty(t, outside)
}

func TestFilesystemTraceStoreRejectsMetadataAndCategorySymlinks(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	createTestRun(t, store, "run-1")
	runDir := filepath.Join(root, "traces", "run-1")
	outside := t.TempDir()
	outsideMetadata := filepath.Join(outside, "metadata.json")
	original := []byte(`{"run_id":"outside"}`)
	if err := os.WriteFile(outsideMetadata, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(runDir, "metadata.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideMetadata, filepath.Join(runDir, "metadata.json")); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMetadataMode("run-1", "plan"); err == nil {
		t.Fatal("UpdateMetadataMode() error = nil, want metadata symlink rejection")
	}
	if runs, err := store.ListRuns(RunFilter{}); err != nil {
		t.Fatal(err)
	} else if len(runs) != 0 {
		t.Fatalf("ListRuns() = %#v, want metadata symlink skipped", runs)
	}
	if _, err := store.CreateRunDir("run-1", RunMetadata{RunID: "run-1"}); err != nil {
		t.Fatalf("CreateRunDir() replacing metadata symlink: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(runDir, "metadata.json")); err != nil {
		t.Fatal(err)
	} else if !info.Mode().IsRegular() {
		t.Fatalf("replacement metadata mode = %v, want regular", info.Mode())
	}
	outsideCategory := filepath.Join(outside, "events.jsonl")
	if err := os.WriteFile(outsideCategory, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideCategory, filepath.Join(runDir, "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTrace("run-1", "events", []byte(`{"inside":true}`)); err == nil {
		t.Fatal("AppendTrace() error = nil, want category symlink rejection")
	}
	got, err := os.ReadFile(outsideMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("outside metadata = %q, want %q", got, original)
	}
	got, err = os.ReadFile(outsideCategory)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside\n" {
		t.Fatalf("outside category = %q, want unchanged", got)
	}
}

func TestFilesystemTraceStoreReplacesHardlinkedAppendTargetWithoutChangingOutsideAlias(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	createTestRun(t, store, "run-1")
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, "traces", "run-1", "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTrace("run-1", "events", []byte(`{"inside":true}`)); err != nil {
		t.Fatalf("AppendTrace() error = %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside\n" {
		t.Fatalf("hardlink contents = %q, want unchanged", got)
	}
	inside, err := os.ReadFile(filepath.Join(root, "traces", "run-1", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(inside) != "{\"inside\":true}\n" {
		t.Fatalf("inside trace = %q", inside)
	}
	rotated, err := os.ReadFile(filepath.Join(root, "traces", "run-1", "events.jsonl.001"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) != "outside\n" {
		t.Fatalf("rotated hardlinked chunk = %q, want original contents", rotated)
	}
}

func TestFilesystemTraceStoreCloseIsIdempotentAndFailsClosed(t *testing.T) {
	store := newTestFilesystemTraceStore(t, t.TempDir())
	createTestRun(t, store, "run-1")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTrace("run-1", "events", []byte(`{}`)); err == nil {
		t.Fatal("AppendTrace() after Close error = nil")
	}
}

func TestFilesystemTraceStoreNormalBehaviorModesAndAtomicReplacement(t *testing.T) {
	root := t.TempDir()
	store := newTestFilesystemTraceStore(t, root)
	started := time.Now().UTC().Truncate(time.Second)
	if _, err := store.CreateRunDir("run-1", RunMetadata{RunID: "run-1", CandidateID: "candidate", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "traces", "run-1")
	metadataPath := filepath.Join(runDir, "metadata.json")
	metadataInode := inodeOf(t, metadataPath)
	if err := store.UpdateMetadataMode("run-1", "plan"); err != nil {
		t.Fatal(err)
	}
	if got := inodeOf(t, metadataPath); got == metadataInode {
		t.Fatal("metadata inode was not replaced")
	}
	if err := store.AppendTrace("run-1", "events", []byte(`{"event":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTrace("run-1", "events", []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(runDir, "events.jsonl")); err != nil {
		t.Fatal(err)
	} else if string(got) != "{\"event\":1}\nsecond\n" {
		t.Fatalf("events = %q", got)
	}
	if err := store.WriteFile("run-1", "nested/artifact.txt", []byte("first")); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(runDir, "nested", "artifact.txt")
	artifactInode := inodeOf(t, artifactPath)
	if err := store.WriteFile("run-1", "nested/artifact.txt", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if got := inodeOf(t, artifactPath); got == artifactInode {
		t.Fatal("WriteFile destination inode was not replaced")
	}
	runs, err := store.ListRuns(RunFilter{CandidateID: "candidate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Mode != "plan" {
		t.Fatalf("ListRuns() = %#v", runs)
	}
	for _, path := range []string{filepath.Join(root, "traces"), runDir, filepath.Join(runDir, "nested")} {
		assertMode(t, path, 0o700)
	}
	for _, path := range []string{metadataPath, filepath.Join(runDir, "events.jsonl"), artifactPath} {
		assertMode(t, path, 0o600)
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata RunMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Mode != "plan" {
		t.Fatalf("metadata mode = %q, want plan", metadata.Mode)
	}
}

func newTestFilesystemTraceStore(t *testing.T, root string) *FilesystemTraceStore {
	t.Helper()
	store, err := NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func createTestRun(t *testing.T, store *FilesystemTraceStore, runID string) {
	t.Helper()
	if _, err := store.CreateRunDir(runID, RunMetadata{RunID: runID, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func assertDirEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory %s contains %v, want empty", path, entries)
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %o, want %o", path, got, want)
	}
}
