package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceWriteFileToolRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(root, "outside.txt")
	input := mustJSON(t, map[string]string{
		"file_path": "../outside.txt",
		"content":   "outside",
	})

	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), input, workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute() IsError = false, content = %q", result.Content)
	}
	if !strings.Contains(result.Content, "outside the workspace root") {
		t.Fatalf("Execute() content = %q, want workspace escape message", result.Content)
	}
	if _, err := os.Stat(outsidePath); !os.IsNotExist(err) {
		t.Fatalf("outside file stat error = %v, want not exist", err)
	}
}

func TestWorkspaceEditToolRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := mustJSON(t, map[string]any{
		"file_path":  "../outside.txt",
		"old_string": "before",
		"new_string": "after",
	})

	result, err := (&WorkspaceEditTool{}).Execute(context.Background(), input, workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute() IsError = false, content = %q", result.Content)
	}
	if !strings.Contains(result.Content, "outside the workspace root") {
		t.Fatalf("Execute() content = %q, want workspace escape message", result.Content)
	}
	content, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "before" {
		t.Fatalf("outside file content = %q, want unchanged", string(content))
	}
}

func TestWorkspaceWriteFileToolRejectsSymlinkDirectoryEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outsideDir := filepath.Join(root, "outside")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	input := mustJSON(t, map[string]string{
		"file_path": filepath.Join("link", "pwned.txt"),
		"content":   "outside",
	})
	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), input, workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "outside the workspace root") {
		t.Fatalf("Execute() = %#v, want workspace escape", result)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file stat error = %v, want not exist", err)
	}
}

func TestWorkspaceWriteFileToolRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outsidePath := filepath.Join(root, "outside.txt")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePath, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(workDir, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	input := mustJSON(t, map[string]string{
		"file_path": "link.txt",
		"content":   "after",
	})
	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), input, workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute() = %#v, want final symlink rejection", result)
	}
	content, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "before" {
		t.Fatalf("outside file content = %q, want unchanged", content)
	}
}

func TestWorkspaceEditToolRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outsidePath := filepath.Join(root, "outside.txt")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePath, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(workDir, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	input := mustJSON(t, map[string]any{
		"file_path":  "link.txt",
		"old_string": "before",
		"new_string": "after",
	})
	result, err := (&WorkspaceEditTool{}).Execute(context.Background(), input, workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute() = %#v, want final symlink rejection", result)
	}
	content, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "before" {
		t.Fatalf("outside file content = %q, want unchanged", content)
	}
}

func TestWorkspaceWriteFileToolRejectsSymlinkInPath(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workDir, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}
	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path": "link.txt",
		"content":   "hijacked",
	}), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute() = %#v, want symlink rejection", result)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before" {
		t.Fatalf("outside file = %q, want unchanged", got)
	}
}

func TestWorkspaceWriteFileToolAcceptsSymlinkedWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	realWorkDir := filepath.Join(root, "real-workspace")
	linkWorkDir := filepath.Join(root, "workspace-link")
	if err := os.Mkdir(realWorkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realWorkDir, linkWorkDir); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}

	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path": filepath.Join("nested", "note.txt"),
		"content":   "inside",
	}), linkWorkDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() = %#v, want success", result)
	}
	got, err := os.ReadFile(filepath.Join(realWorkDir, "nested", "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "inside" {
		t.Fatalf("written file = %q, want inside", got)
	}
}

func TestWorkspaceEditToolRejectsSymlinkInPath(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workDir, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}
	result, err := (&WorkspaceEditTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path":  "link.txt",
		"old_string": "before",
		"new_string": "after",
	}), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute() = %#v, want symlink rejection", result)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before" {
		t.Fatalf("outside file = %q, want unchanged", got)
	}
}

func TestWorkspaceEditToolAcceptsSymlinkedWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	realWorkDir := filepath.Join(root, "real-workspace")
	linkWorkDir := filepath.Join(root, "workspace-link")
	if err := os.Mkdir(realWorkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realWorkDir, "note.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realWorkDir, linkWorkDir); err != nil {
		t.Skipf("symlink creation unsupported: %v", err)
	}

	result, err := (&WorkspaceEditTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path":  "note.txt",
		"old_string": "before",
		"new_string": "after",
	}), linkWorkDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() = %#v, want success", result)
	}
	got, err := os.ReadFile(filepath.Join(realWorkDir, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Fatalf("edited file = %q, want after", got)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
