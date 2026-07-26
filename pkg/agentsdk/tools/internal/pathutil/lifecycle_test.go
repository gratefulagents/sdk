package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMoveAndDeleteFileInWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("lifecycle primitives fail closed outside Linux")
	}
	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "source.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MoveFileInWorkspace(workDir, "source.txt", "target/destination.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "source.txt")); !os.IsNotExist(err) {
		t.Fatalf("source stat = %v, want not exist", err)
	}
	if err := DeleteFileInWorkspace(workDir, "target/destination.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "target/destination.txt")); !os.IsNotExist(err) {
		t.Fatalf("destination stat = %v, want not exist", err)
	}
}

func TestMoveAndRecursiveDeleteDirectoryInWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("lifecycle primitives fail closed outside Linux")
	}
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "source", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "source", "nested", "file.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MovePathInWorkspace(workDir, "source", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := DeletePathInWorkspace(workDir, "renamed", false); err == nil {
		t.Fatal("DeletePathInWorkspace() deleted non-empty directory without recursive")
	}
	if err := DeletePathInWorkspace(workDir, "renamed/nested/file.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := DeletePathInWorkspace(workDir, "renamed/nested", false); err != nil {
		t.Fatal(err)
	}
	if err := DeletePathInWorkspace(workDir, "renamed", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "renamed")); !os.IsNotExist(err) {
		t.Fatalf("renamed stat = %v, want not exist", err)
	}
}

func TestLifecycleOperationsRejectEscapeAndHardlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("lifecycle primitives fail closed outside Linux")
	}
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	inside := filepath.Join(workDir, "inside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, inside); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if err := DeleteFileInWorkspace(workDir, "inside.txt"); err == nil {
		t.Fatal("DeleteFileInWorkspace() accepted hardlink")
	}
	if err := MoveFileInWorkspace(workDir, "inside.txt", "../escaped.txt"); err == nil {
		t.Fatal("MoveFileInWorkspace() accepted workspace escape")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "outside" {
		t.Fatalf("outside = %q, want unchanged", data)
	}
}
