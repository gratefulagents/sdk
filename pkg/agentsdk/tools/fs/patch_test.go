package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestApplyPatchDryRunDoesNotMutate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	file := filepath.Join(workDir, "note.txt")
	if err := os.WriteFile(file, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := executePatch(t, workDir, `diff --git a/note.txt b/note.txt
--- a/note.txt
+++ b/note.txt
@@ -1 +1 @@
-before
+after
`, true)
	if result.IsError {
		t.Fatalf("Execute() = %#v", result)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before\n" {
		t.Fatalf("file = %q, want unchanged", data)
	}
	var audit patchResult
	if err := json.Unmarshal([]byte(result.Content), &audit); err != nil {
		t.Fatal(err)
	}
	if !audit.DryRun || len(audit.Operations) != 1 || audit.Operations[0].Operation != "modify" {
		t.Fatalf("audit = %#v", audit)
	}
}

func TestApplyPatchMultiFileSuccess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "existing.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := executePatch(t, workDir, `diff --git a/existing.txt b/existing.txt
--- a/existing.txt
+++ b/existing.txt
@@ -1 +1 @@
-one
+two
diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+created
`, false)
	if result.IsError {
		t.Fatalf("Execute() = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "existing.txt"), "two\n")
	assertFileContent(t, filepath.Join(workDir, "new.txt"), "created\n")
}

func TestApplyPatchValidationFailureDoesNotMutateAnyFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	first := filepath.Join(workDir, "first.txt")
	second := filepath.Join(workDir, "second.txt")
	if err := os.WriteFile(first, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := executePatch(t, workDir, `diff --git a/first.txt b/first.txt
--- a/first.txt
+++ b/first.txt
@@ -1 +1 @@
-one
+changed
diff --git a/second.txt b/second.txt
--- a/second.txt
+++ b/second.txt
@@ -1 +1 @@
-wrong
+changed
`, false)
	if !result.IsError || !strings.Contains(result.Content, "hunk does not match") {
		t.Fatalf("Execute() = %#v", result)
	}
	assertFileContent(t, first, "one\n")
	assertFileContent(t, second, "two\n")
}

func TestApplyPatchOpenAIEnvelopeMultiFileMultiHunk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, "first.txt", "one\ntwo\nthree\nfour\n", 0o644)
	writePatchTestFile(t, workDir, "second.txt", "alpha\nbeta\ngamma\n", 0o644)
	result := executePatch(t, workDir, `*** Begin Patch
*** Update File: first.txt
@@
 one
-two
+two changed
 three
@@
 three
-four
+four changed
*** Update File: second.txt
@@
 alpha
-beta
+BETA
 gamma
*** End Patch`, false)
	if result.IsError {
		t.Fatalf("Execute() = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "first.txt"), "one\ntwo changed\nthree\nfour changed\n")
	assertFileContent(t, filepath.Join(workDir, "second.txt"), "alpha\nBETA\ngamma\n")
}

func TestApplyPatchOpenAIEnvelopeMismatchIsAtomic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	first := filepath.Join(workDir, "first.txt")
	second := filepath.Join(workDir, "second.txt")
	writePatchTestFile(t, workDir, "first.txt", "before\n", 0o644)
	writePatchTestFile(t, workDir, "second.txt", "actual\n", 0o644)
	result := executePatch(t, workDir, `*** Begin Patch
*** Update File: first.txt
@@
-before
+after
*** Update File: second.txt
@@
-expected
+changed
*** End Patch`, false)
	if !result.IsError || !strings.Contains(result.Content, "does not match") {
		t.Fatalf("Execute() = %#v", result)
	}
	assertFileContent(t, first, "before\n")
	assertFileContent(t, second, "actual\n")
}

func TestApplyPatchOpenAIEnvelopeAddDeleteAndMove(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, "delete.txt", "remove\n", 0o644)
	writePatchTestFile(t, workDir, "old.txt", "before\n", 0o644)
	result := executePatch(t, workDir, `*** Begin Patch
*** Add File: added.txt
+created
+line
*** Delete File: delete.txt
*** Update File: old.txt
*** Move to: moved.txt
@@
-before
+after
*** End Patch`, false)
	if result.IsError {
		t.Fatalf("Execute() = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "added.txt"), "created\nline\n")
	if _, err := os.Stat(filepath.Join(workDir, "delete.txt")); !os.IsNotExist(err) {
		t.Fatalf("delete.txt stat = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt stat = %v, want not exist", err)
	}
	assertFileContent(t, filepath.Join(workDir, "moved.txt"), "after\n")
}

func TestApplyPatchRejectsMalformedOrEscapingOpenAIEnvelope(t *testing.T) {
	workDir := t.TempDir()
	outside := filepath.Join(filepath.Dir(workDir), "outside.txt")
	for _, patch := range []string{
		"*** Begin Patch\n*** Update File: note.txt\n@@\n-old\n+new\n",
		"*** Begin Patch\n*** Update File: note.txt\n@@\n*** End Patch",
		"*** Begin Patch\n*** Add File: note.txt\nraw\n*** End Patch",
		"*** Begin Patch\n*** Update File: ../outside.txt\n@@\n-old\n+new\n*** End Patch",
	} {
		result := executePatch(t, workDir, patch, false)
		if !result.IsError {
			t.Fatalf("patch unexpectedly accepted: %q", patch)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside stat = %v, want not exist", err)
	}
}

func TestApplyPatchOpenAIEnvelopeRejectsAmbiguousHunk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, "ambiguous.txt", "same\nsame\n", 0o644)
	result := executePatch(t, workDir, `*** Begin Patch
*** Update File: ambiguous.txt
@@
-same
+changed
*** End Patch`, false)
	if !result.IsError || !strings.Contains(result.Content, "more than once") {
		t.Fatalf("ambiguous patch = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "ambiguous.txt"), "same\nsame\n")
}

func TestApplyPatchOpenAIEnvelopeRejectsEmptyPathsAtomically(t *testing.T) {
	workDir := t.TempDir()
	file := filepath.Join(workDir, "first.txt")
	writePatchTestFile(t, workDir, "first.txt", "before\n", 0o644)
	for _, suffix := range []string{
		"*** Update File: \n@@\n-before\n+after\n",
		"*** Add File: \n+created\n",
		"*** Delete File: \n",
		"*** Move to: \n",
	} {
		patch := "*** Begin Patch\n*** Update File: first.txt\n@@\n-before\n+after\n" + suffix + "*** End Patch\n"
		result := executePatch(t, workDir, patch, false)
		if !result.IsError {
			t.Fatalf("empty path patch unexpectedly accepted: %q", suffix)
		}
		assertFileContent(t, file, "before\n")
	}
}

func TestApplyPatchOpenAIEnvelopeGrammarVariants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, "located.txt", "header\nlocator text\nbefore\n", 0o644)
	writePatchTestFile(t, workDir, "direct.txt", "old\n", 0o644)
	writePatchTestFile(t, workDir, "eof.txt", "last\n", 0o644)
	result := executePatch(t, workDir, "*** Begin Patch\n*** Update File: located.txt\n@@ locator text\n-before\n+after\n*** Update File: direct.txt\n-old\n+new\n*** Update File: eof.txt\n@@\n-last\n+final\n*** End of File\n*** End Patch\n", false)
	if result.IsError {
		t.Fatalf("grammar variant patch = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "located.txt"), "header\nlocator text\nafter\n")
	assertFileContent(t, filepath.Join(workDir, "direct.txt"), "new\n")
	assertFileContent(t, filepath.Join(workDir, "eof.txt"), "final\n")

	writePatchTestFile(t, workDir, "crlf.txt", "old\r\n", 0o644)
	result = executePatch(t, workDir, "*** Begin Patch\r\n*** Update File: crlf.txt\r\n@@\r\n-old\r\n+new\r\n*** End Patch\r\n", false)
	if result.IsError {
		t.Fatalf("CRLF patch = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "crlf.txt"), "new\r\n")
}

func TestApplyPatchOpenAIEnvelopeRejectsSuffixAndOutOfOrderHunks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, "suffix.txt", "prefix-old\n", 0o644)
	result := executePatch(t, workDir, "*** Begin Patch\n*** Update File: suffix.txt\n@@\n-old\n+new\n*** End Patch\n", false)
	if !result.IsError || !strings.Contains(result.Content, "does not match") {
		t.Fatalf("suffix patch = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "suffix.txt"), "prefix-old\n")

	writePatchTestFile(t, workDir, "ordered.txt", "one\ntwo\nthree\nfour\n", 0o644)
	result = executePatch(t, workDir, "*** Begin Patch\n*** Update File: ordered.txt\n@@\n-three\n+THREE\n@@\n-one\n+ONE\n*** End Patch\n", false)
	if !result.IsError || !strings.Contains(result.Content, "does not match") {
		t.Fatalf("out-of-order patch = %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "ordered.txt"), "one\ntwo\nthree\nfour\n")
}

func TestApplyPatchOpenAIEnvelopeBoundsHunksAndLines(t *testing.T) {
	workDir := t.TempDir()
	tooManyLines := "*** Begin Patch\n*** Add File: bounded.txt\n" + strings.Repeat("+x\n", maxOpenAIHunkLines+1) + "*** End Patch\n"
	if result := executePatch(t, workDir, tooManyLines, false); !result.IsError {
		t.Fatalf("oversized OpenAI hunk unexpectedly accepted: %#v", result)
	}
	tooManyHunks := "*** Begin Patch\n*** Update File: missing.txt\n" + strings.Repeat("@@\n-x\n+y\n", maxOpenAIPatchHunks+1) + "*** End Patch\n"
	if result := executePatch(t, workDir, tooManyHunks, false); !result.IsError {
		t.Fatalf("too many OpenAI hunks unexpectedly accepted: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(workDir, "bounded.txt")); !os.IsNotExist(err) {
		t.Fatalf("bounded.txt stat = %v, want not exist", err)
	}
}

func TestApplyRangeLessPatchHunksRepeatedPrefix(t *testing.T) {
	content := strings.Repeat("a\n", maxPatchedFileBytes/2)
	lines := make([]patchLine, maxOpenAIHunkLines)
	for i := range lines {
		lines[i] = patchLine{kind: ' ', text: "a"}
	}
	lines[len(lines)-1] = patchLine{kind: '-', text: "b"}

	_, err := applyRangeLessPatchHunks(content, []patchHunk{{
		rangeLess: true,
		oldCount:  len(lines),
		newCount:  len(lines) - 1,
		lines:     lines,
	}})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("applyRangeLessPatchHunks() error = %v, want no match", err)
	}
}

func TestApplyPatchRejectsEscapesBinaryAndOversize(t *testing.T) {
	workDir := t.TempDir()
	outside := filepath.Join(filepath.Dir(workDir), "outside.txt")
	for _, patch := range []string{
		"diff --git a/../outside.txt b/../outside.txt\n--- a/../outside.txt\n+++ b/../outside.txt\n@@ -0,0 +1 @@\n+x\n",
		"diff --git a/a b/a\nGIT binary patch\n\x00",
		strings.Repeat("x", maxPatchBytes+1),
	} {
		result := executePatch(t, workDir, patch, false)
		if !result.IsError {
			t.Fatalf("patch accepted: %q", patch[:min(len(patch), 40)])
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside stat = %v, want not exist", err)
	}
}

func TestApplyPatchRejectsFIFOWithoutBlocking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workDir, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan agentsdk.ToolResult, 1)
	go func() {
		input, _ := json.Marshal(map[string]any{"patch": `diff --git a/pipe b/pipe
--- a/pipe
+++ b/pipe
@@ -0,0 +1 @@
+data
`, "dry_run": true})
		result, _ := (&ApplyPatchTool{}).Execute(context.Background(), input, workDir)
		done <- result
	}()
	select {
	case result := <-done:
		if !result.IsError {
			t.Fatalf("FIFO patch unexpectedly accepted: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO patch blocked")
	}
}

func TestApplyPatchRejectsSymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := executePatch(t, workDir, `diff --git a/link.txt b/link.txt
--- a/link.txt
+++ b/link.txt
@@ -1 +1 @@
-before
+after
`, false)
	if !result.IsError {
		t.Fatalf("symlink patch = %#v, want refusal", result)
	}
	assertFileContent(t, outside, "before\n")
}

func TestApplyPatchChangesExecutableBit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	file := filepath.Join(workDir, "script.sh")
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := executePatch(t, workDir, `diff --git a/script.sh b/script.sh
old mode 100644
new mode 100755
`, false)
	if result.IsError {
		t.Fatalf("Execute() = %#v", result)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("mode = %o, want executable", info.Mode().Perm())
	}
}

func TestApplyPatchRenameAndDelete(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "old.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "delete.txt"), []byte("remove\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := executePatch(t, workDir, `diff --git a/old.txt b/new.txt
similarity index 100%
rename from old.txt
rename to new.txt
diff --git a/delete.txt b/delete.txt
deleted file mode 100644
--- a/delete.txt
+++ /dev/null
@@ -1 +0,0 @@
-remove
`, false)
	if result.IsError {
		t.Fatalf("Execute() = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(workDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old file stat = %v, want not exist", err)
	}
	assertFileContent(t, filepath.Join(workDir, "new.txt"), "keep\n")
	if _, err := os.Stat(filepath.Join(workDir, "delete.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file stat = %v, want not exist", err)
	}
}

func TestApplyPatchRejectsUnsupportedCopyAndExcessHunkLines(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, "source.txt", "one\n", 0o644)
	copyPatch := `diff --git a/source.txt b/copied.txt
similarity index 100%
copy from source.txt
copy to copied.txt
`
	if result := executePatch(t, workDir, copyPatch, true); !result.IsError {
		t.Fatalf("copy patch unexpectedly accepted: %#v", result)
	}
	excess := `diff --git a/source.txt b/source.txt
--- a/source.txt
+++ b/source.txt
@@ -1 +1 @@
-one
+two
+unexpected
`
	if result := executePatch(t, workDir, excess, true); !result.IsError {
		t.Fatalf("excess hunk line unexpectedly accepted: %#v", result)
	}
	truncatedRename := "diff --git a/source.txt b/renamed.txt\n"
	if result := executePatch(t, workDir, truncatedRename, true); !result.IsError {
		t.Fatalf("truncated rename unexpectedly accepted: %#v", result)
	}
	nonCanonicalMode := "diff --git a/source.txt b/source.txt\nold mode 100644\nnew mode 100600\n"
	if result := executePatch(t, workDir, nonCanonicalMode, true); !result.IsError {
		t.Fatalf("non-canonical Git mode unexpectedly accepted: %#v", result)
	}
	ancestorConflict := `diff --git a/a b/a
new file mode 100644
--- /dev/null
+++ b/a
@@ -0,0 +1 @@
+file
diff --git a/a/b b/a/b
new file mode 100644
--- /dev/null
+++ b/a/b
@@ -0,0 +1 @@
+nested
`
	if result := executePatch(t, workDir, ancestorConflict, false); !result.IsError {
		t.Fatalf("ancestor conflict unexpectedly accepted: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(workDir, "a")); !os.IsNotExist(err) {
		t.Fatalf("failed ancestor patch left path behind: %v", err)
	}
}

func TestApplyPatchRenamePreservesLiteralATopLevelDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, filepath.Join("a", "old.txt"), "data\n", 0o644)
	patch := `diff --git a/a/old.txt b/a/new.txt
similarity index 100%
rename from a/old.txt
rename to a/new.txt
`
	result := executePatch(t, workDir, patch, false)
	if result.IsError {
		t.Fatalf("rename patch failed: %#v", result)
	}
	assertFileContent(t, filepath.Join(workDir, "a", "new.txt"), "data\n")
}

func TestApplyPatchRejectsHardlinkAndConflictingPaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	inside := filepath.Join(workDir, "inside.txt")
	if err := os.WriteFile(outside, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, inside); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	result := executePatch(t, workDir, `diff --git a/inside.txt b/inside.txt
--- a/inside.txt
+++ b/inside.txt
@@ -1 +1 @@
-before
+after
`, false)
	if !result.IsError || !strings.Contains(result.Content, "hard link") {
		t.Fatalf("hardlink patch = %#v", result)
	}
	assertFileContent(t, outside, "before\n")

	result = executePatch(t, workDir, `diff --git a/a.txt b/b.txt
similarity index 100%
rename from a.txt
rename to b.txt
diff --git a/b.txt b/c.txt
similarity index 100%
rename from b.txt
rename to c.txt
`, false)
	if !result.IsError || !strings.Contains(result.Content, "conflicting patch paths") {
		t.Fatalf("conflicting patch = %#v", result)
	}
}

func TestMoveAndDeleteToolsSupportDirectories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace lifecycle operations fail closed outside Linux")
	}
	workDir := t.TempDir()
	writePatchTestFile(t, workDir, filepath.Join("tree", "nested", "file.txt"), "data\n", 0o644)
	move, err := (&MoveTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"source_path": "tree", "destination_path": "renamed",
	}), workDir)
	if err != nil || move.IsError {
		t.Fatalf("Move() = %#v, %v", move, err)
	}
	refusal, err := (&DeleteTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"path": "renamed"}), workDir)
	if err != nil || !refusal.IsError {
		t.Fatalf("non-empty directory Delete() = %#v, %v", refusal, err)
	}
	for _, path := range []string{"renamed/nested/file.txt", "renamed/nested", "renamed"} {
		deleted, err := (&DeleteTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"path": path}), workDir)
		if err != nil || deleted.IsError {
			t.Fatalf("Delete(%s) = %#v, %v", path, deleted, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, "renamed")); !os.IsNotExist(err) {
		t.Fatalf("renamed stat = %v, want not exist", err)
	}
}

func TestMoveAndDeleteToolsAreConfinedAndRejectHardlinks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace mutation tools fail closed outside Linux")
	}
	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "from.txt"), []byte("move\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	move, err := (&MoveTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"source_path": "from.txt", "destination_path": "target/to.txt",
	}), workDir)
	if err != nil || move.IsError {
		t.Fatalf("Move() = %#v, %v", move, err)
	}
	assertFileContent(t, filepath.Join(workDir, "target", "to.txt"), "move\n")
	deleted, err := (&DeleteTool{}).Execute(context.Background(), mustJSON(t, map[string]string{"path": "target/to.txt"}), workDir)
	if err != nil || deleted.IsError {
		t.Fatalf("Delete() = %#v, %v", deleted, err)
	}

	if err := os.WriteFile(filepath.Join(workDir, "escape.txt"), []byte("stay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	escape, err := (&MoveTool{}).Execute(context.Background(), mustJSON(t, map[string]string{
		"source_path": "escape.txt", "destination_path": "../outside.txt",
	}), workDir)
	if err != nil || !escape.IsError {
		t.Fatalf("escape Move() = %#v, %v", escape, err)
	}

	outside := filepath.Join(filepath.Dir(workDir), "outside.txt")
	hardlink := filepath.Join(workDir, "hardlink.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, hardlink); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	refusal, err := (&DeleteTool{}).Execute(context.Background(), mustJSON(t, map[string]string{"path": "hardlink.txt"}), workDir)
	if err != nil || !refusal.IsError || !strings.Contains(refusal.Content, "hard link") {
		t.Fatalf("hardlink Delete() = %#v, %v", refusal, err)
	}
	assertFileContent(t, outside, "outside\n")
}

func executePatch(t *testing.T, workDir, patch string, dryRun bool) agentsdk.ToolResult {
	t.Helper()
	result, err := (&ApplyPatchTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"patch":   patch,
		"dry_run": dryRun,
	}), workDir)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertFileContent(t *testing.T, file, want string) {
	t.Helper()
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("file %s = %q, want %q", file, got, want)
	}
}

func writePatchTestFile(t *testing.T, root, relative, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
