package tracestore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilesystemTraceStoreRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.CreateRunDir("../escape", RunMetadata{RunID: "../escape", StartedAt: time.Now()}); err == nil {
		t.Fatal("CreateRunDir() error = nil, want invalid run id")
	}
	if err := store.CreateRunDirMust("run-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTrace("run-1", "../llm_calls", []byte(`{}`)); err == nil {
		t.Fatal("AppendTrace() error = nil, want invalid category")
	}
	if err := store.WriteFile("run-1", "../outside.txt", []byte("escape")); err == nil {
		t.Fatal("WriteFile() error = nil, want invalid relative path")
	}
	if _, err := os.Stat(filepath.Join(root, "outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file stat error = %v, want not exist", err)
	}
}

func TestTraceWriterRedactsObviousSecrets(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	writer := NewTraceWriter(store)
	if err := writer.InitRun(RunMetadata{RunID: "run-1", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	writer.appendJSON("tool_calls", map[string]any{
		"authorization": "Bearer sk-testsecret",
		"body":          `{"refresh_token":"refresh-secret"}`,
	})
	data, err := os.ReadFile(filepath.Join(root, "traces", "run-1", "tool_calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, secret := range []string{"sk-testsecret", "refresh-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("trace leaked secret %q in %s", secret, text)
		}
	}
}

func (s *FilesystemTraceStore) CreateRunDirMust(runID string) error {
	_, err := s.CreateRunDir(runID, RunMetadata{RunID: runID, StartedAt: time.Now()})
	return err
}
