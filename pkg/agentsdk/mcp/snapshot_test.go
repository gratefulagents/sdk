package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigSnapshot_PinsPathAndContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"a":{"command":"/bin/true"}}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	snap, err := LoadConfigSnapshot(path)
	if err != nil {
		t.Fatalf("LoadConfigSnapshot: %v", err)
	}
	if snap.AbsPath == "" || !filepath.IsAbs(snap.AbsPath) {
		t.Fatalf("AbsPath not pinned: %q", snap.AbsPath)
	}
	if len(snap.ContentSHA256) == 0 {
		t.Fatalf("ContentSHA256 empty")
	}
	if _, ok := snap.Config.MCPServers["a"]; !ok {
		t.Fatalf("config not loaded: %#v", snap.Config)
	}
}

func TestVerifyUnchanged_DetectsModification(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap, err := LoadConfigSnapshot(path)
	if err != nil {
		t.Fatalf("LoadConfigSnapshot: %v", err)
	}

	// Mutate the file (simulating an in-run swap) and verify reload is refused.
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"evil":{"command":"/bin/true"}}}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := snap.VerifyUnchanged(); err == nil {
		t.Fatalf("VerifyUnchanged expected error after mutation, got nil")
	} else if !strings.Contains(err.Error(), "modified") {
		t.Fatalf("error should mention modification: %v", err)
	}
}

func TestLoadConfigSnapshot_FlagsAgentWritablePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // simulated agent workspace
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	snap, err := LoadConfigSnapshotInWorkspace(path, dir)
	if err != nil {
		t.Fatalf("LoadConfigSnapshotInWorkspace: %v", err)
	}
	if !snap.AgentWritable {
		t.Fatalf("expected AgentWritable=true for path inside workspace")
	}
}
