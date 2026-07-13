package search

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListFilesTool(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "b.txt", "bravo")
	writeTestFile(t, dir, "a.txt", "alpha")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := (&ListFilesTool{}).Execute(context.Background(), json.RawMessage(`{"path":".","limit":10}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := result.Content, "a.txt\nb.txt\nsub/"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
}

func TestReadFileToolLineRange(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "notes.txt", "one\ntwo\nthree\n")

	result, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"notes.txt","start_line":2,"end_line":3}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := result.Content, "two\nthree"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
}

func TestReadFileToolInvertedLineRangeReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "notes.txt", "one\ntwo\nthree\nfour\n")

	result, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"notes.txt","start_line":4,"end_line":2}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Content != "" {
		t.Fatalf("Content = %q, want empty result", result.Content)
	}
}

func TestReadFileToolCapsLargeFilesBeforeReadingAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.txt")
	data := strings.Repeat("a", maxReadBytes+2048)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"huge.txt"}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result.Content, "[output truncated]") {
		t.Fatalf("Content missing truncation marker")
	}
	if len(result.Content) > maxReadBytes+len("\n[output truncated]") {
		t.Fatalf("Content len = %d, want bounded output", len(result.Content))
	}
}

func TestGlobTool(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "main.go", "package main")
	writeTestFile(t, dir, filepath.Join("pkg", "lib.go"), "package pkg")
	writeTestFile(t, dir, filepath.Join("pkg", "readme.md"), "docs")

	result, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := result.Content, "main.go\npkg/lib.go"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
}

func TestGrepTool(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "hello\nneedle\n")
	writeTestFile(t, dir, filepath.Join("nested", "b.txt"), "Needle case\n")

	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","ignore_case":true}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result.Content, "a.txt:2: needle") || !strings.Contains(result.Content, "nested/b.txt:1: Needle case") {
		t.Fatalf("Content missing matches: %q", result.Content)
	}
}

func TestGrepToolDoesNotFollowSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside.txt")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("needle outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "leak.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := result.Content, "(no matches)"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
}

func TestWorkspacePathRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	_, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"../outside.txt"}`), dir)
	if err == nil {
		t.Fatal("Execute() error = nil, want workspace escape error")
	}
}

func TestReadFileToolRejectsSymlinkEscape(t *testing.T) {
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

	_, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"leak.txt"}`), workDir)
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("Execute() error = %v, want workspace escape", err)
	}
}

func TestReadFileToolRejectsHardlinkAlias(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(workDir, "alias.txt")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	_, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"alias.txt"}`), workDir)
	if err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("Execute() error = %v, want hardlink refusal", err)
	}
}

func TestListFilesToolReturnsErrorOnInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	_, err := (&ListFilesTool{}).Execute(context.Background(), json.RawMessage(`{not-json`), dir)
	if err == nil {
		t.Fatal("Execute() error = nil, want JSON unmarshal error")
	}
}

func TestGlobToolReturnsErrorOnInvalidPattern(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "hi")
	result, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"[abc"}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "invalid glob pattern") {
		t.Fatalf("result = %#v, want invalid glob pattern error", result)
	}
}

func TestGrepToolReturnsErrorOnInvalidGlob(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "hi")
	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"hi","glob":"[abc"}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "invalid glob filter") {
		t.Fatalf("result = %#v, want invalid glob filter error", result)
	}
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGrepToolSkipsFilesWithTooLongLines(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "ok.txt", "needle here\n")
	writeTestFile(t, dir, "huge.txt", "needle "+strings.Repeat("x", 2*1024*1024)+"\n")

	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content = %q", result.Content)
	}
	if !strings.Contains(result.Content, "ok.txt:1: needle here") {
		t.Fatalf("Content = %q, want match from ok.txt", result.Content)
	}
	if !strings.Contains(result.Content, "[skipped huge.txt: line too long]") {
		t.Fatalf("Content = %q, want skip note for huge.txt", result.Content)
	}
}

func TestWorkspacePathAcceptsAbsolutePathInsideWorkdir(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, filepath.Join("sub", "notes.txt"), "hello\n")

	input, err := json.Marshal(map[string]any{"path": filepath.Join(dir, "sub", "notes.txt")})
	if err != nil {
		t.Fatal(err)
	}
	result, execErr := (&ReadFileTool{}).Execute(context.Background(), input, dir)
	if execErr != nil {
		t.Fatalf("Execute() error = %v", execErr)
	}
	if result.IsError || result.Content != "hello\n" {
		t.Fatalf("result = %#v, want file contents", result)
	}
}

func TestWorkspacePathRejectsAbsolutePathOutsideWorkdir(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "elsewhere.txt")

	input, err := json.Marshal(map[string]any{"path": outside})
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := (&ReadFileTool{}).Execute(context.Background(), input, dir)
	if execErr == nil || !strings.Contains(execErr.Error(), "path must be relative to workdir") {
		t.Fatalf("Execute() error = %v, want path must be relative to workdir", execErr)
	}
}

func TestReadFileToolNotFoundSuggestsCandidates(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, filepath.Join("cfg", "config.yaml"), "a: 1\n")

	result, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"config.yaml"}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, content = %q", result.Content)
	}
	want := "config.yaml: no such file — did you mean one of: " + filepath.Join("cfg", "config.yaml")
	if result.Content != want {
		t.Fatalf("Content = %q, want %q", result.Content, want)
	}
}

func TestReadFileToolNotFoundWithoutCandidates(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "other.txt", "x")

	result, err := (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"zzz.bin"}`), dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, content = %q", result.Content)
	}
	if got, want := result.Content, "zzz.bin: no such file — use glob to locate the file"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
}
