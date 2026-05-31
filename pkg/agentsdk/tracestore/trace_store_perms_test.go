package tracestore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFilesystemTraceStorePermissions ensures that all files created by the
// trace store are 0o600 and all directories are 0o700, since traces capture
// full conversations, tool I/O, and system prompts.
func TestFilesystemTraceStorePermissions(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRunDir("run-1", RunMetadata{RunID: "run-1", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTrace("run-1", "llm_calls", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile("run-1", "sub/dir/artifact.txt", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteScore("run-1", Score{TaskID: "t", CandidateID: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMetadataFinishedAt("run-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		mode := info.Mode().Perm()
		if info.IsDir() {
			if mode != 0o700 {
				t.Errorf("dir %s mode = %o, want 0o700", path, mode)
			}
		} else {
			if mode != 0o600 {
				t.Errorf("file %s mode = %o, want 0o600", path, mode)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
