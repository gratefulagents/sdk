package pathutil

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkspaceRejectsSymlinkFileEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside.txt")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workDir, "leak.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := ResolveWorkspace(workDir, "leak.txt")
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("ResolveWorkspace() error = %v, want workspace escape", err)
	}
}

func TestResolveWorkspaceRejectsSymlinkDirectoryEscapeForMissingChild(t *testing.T) {
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

	_, err := ResolveWorkspace(workDir, filepath.Join("link", "new.txt"))
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("ResolveWorkspace() error = %v, want workspace escape", err)
	}
}

func TestResolveWorkspaceReturnsCanonicalPathForAllowedSymlink(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	realDir := filepath.Join(workDir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(workDir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := ResolveWorkspace(workDir, filepath.Join("link", "new.txt"))
	if err != nil {
		t.Fatalf("ResolveWorkspace() error = %v", err)
	}
	realCanonical, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realCanonical, "new.txt")
	if got != want {
		t.Fatalf("ResolveWorkspace() = %q, want canonical path %q", got, want)
	}
}

func TestOpenInWorkspaceOpensRegularFile(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workDir, "file.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenInWorkspace(workDir, "file.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenInWorkspace() error = %v", err)
	}
	defer f.Close()
	buf := make([]byte, 5)
	if _, err := f.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("read = %q, want %q", buf, "hello")
	}
}

func TestOpenInWorkspaceAllowsDotDotPrefixPathNames(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	dir := filepath.Join(workDir, "..allowed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenInWorkspace(workDir, target, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenInWorkspace() error = %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("read = %q, want hello", data)
	}
}

func TestOpenInWorkspaceRejectsFinalSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside.txt")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "leak.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := OpenInWorkspace(workDir, "leak.txt", os.O_RDONLY, 0); err == nil {
		t.Fatal("OpenInWorkspace() succeeded through symlink escape")
	}
}

func TestOpenInWorkspaceRejectsIntermediateSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outsideDir := filepath.Join(root, "outside")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := OpenInWorkspace(workDir, filepath.Join("link", "secret.txt"), os.O_RDONLY, 0); err == nil {
		t.Fatal("OpenInWorkspace() succeeded through intermediate symlink escape")
	}
}

func TestWriteFileNoFollowRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := WriteFileNoFollow(link, []byte("changed"), 0o644)
	if err == nil {
		t.Fatal("WriteFileNoFollow() succeeded through symlink")
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "original" {
		t.Fatalf("target content = %q, want original", data)
	}
}
