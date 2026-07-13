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

func TestWorkspaceWriteFileToolReplacesHardlinkWithoutChangingOutsideAlias(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	inside := filepath.Join(workDir, "inside.txt")
	if err := os.WriteFile(outside, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, inside); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path": "inside.txt",
		"content":   "after",
	}), workDir)
	if err != nil || result.IsError {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	outsideData, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(outsideData) != "before" {
		t.Fatalf("outside alias = %q, want before", outsideData)
	}
}

func TestWorkspaceWriteFileToolPreservesExistingMode(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "private.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path": "private.txt",
		"content":   "after",
	}), workDir)
	if err != nil || result.IsError {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestWorkspaceEditToolRejectsHardlinkAlias(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	inside := filepath.Join(workDir, "inside.txt")
	if err := os.WriteFile(outside, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, inside); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	result, err := (&WorkspaceEditTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path":  "inside.txt",
		"old_string": "before",
		"new_string": "after",
	}), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "hard link") {
		t.Fatalf("Execute() = %#v, want hardlink refusal", result)
	}
	outsideData, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(outsideData) != "before" {
		t.Fatalf("outside alias = %q, want before", outsideData)
	}
	insideData, err := os.ReadFile(inside)
	if err != nil {
		t.Fatal(err)
	}
	if string(insideData) != "before" {
		t.Fatalf("inside alias = %q, want unchanged", insideData)
	}
}

func TestWorkspaceWriteFileToolRejectsSymlinkParentDuringCreation(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "parent")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result, err := (&WorkspaceWriteFileTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"file_path": filepath.Join("parent", "nested", "file.txt"),
		"content":   "outside",
	}), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("Execute() = %#v, want error", result)
	}
	if _, err := os.Stat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
		t.Fatalf("outside nested stat = %v, want not exist", err)
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

func TestEditToolsIdenticalStringsMessage(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := mustJSON(t, map[string]any{
		"file_path":  "a.txt",
		"old_string": "hello",
		"new_string": "hello",
	})
	want := "old_string and new_string are identical — if the file already shows the desired text, the edit is already applied; re-read the file before retrying"

	for _, tc := range []struct {
		name string
		run  func() (string, bool)
	}{
		{"FileEditTool", func() (string, bool) {
			r, err := (&FileEditTool{}).Execute(context.Background(), input, workDir)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			return r.Content, r.IsError
		}},
		{"WorkspaceEditTool", func() (string, bool) {
			r, err := (&WorkspaceEditTool{}).Execute(context.Background(), input, workDir)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			return r.Content, r.IsError
		}},
	} {
		content, isErr := tc.run()
		if !isErr || content != want {
			t.Fatalf("%s: result = (%q, %v), want identical-strings guidance", tc.name, content, isErr)
		}
	}
}
