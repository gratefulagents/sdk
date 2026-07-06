package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEditDiffSingleLineChange(t *testing.T) {
	content := "a\nb\nc\nd\ne\nf\ng\nh\n"
	got := buildEditDiff(content, "d", "D", false)
	want := strings.Join([]string{
		"@@ -1,7 +1,7 @@",
		" a",
		" b",
		" c",
		"-d",
		"+D",
		" e",
		" f",
		" g",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffMultiLineReplacementChangesLineCount(t *testing.T) {
	content := "a\nb\nc\nd\ne\nf\ng\nh\n"
	got := buildEditDiff(content, "c\nd", "X", false)
	want := strings.Join([]string{
		"@@ -1,7 +1,6 @@",
		" a",
		" b",
		"-c",
		"-d",
		"+X",
		" e",
		" f",
		" g",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffPartialLineMatchExpandsToFullLines(t *testing.T) {
	content := "func foo() {\n\treturn oldValue\n}\n"
	got := buildEditDiff(content, "oldValue", "newValue", false)
	want := strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" func foo() {",
		"-\treturn oldValue",
		"+\treturn newValue",
		" }",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffReplaceAllDistantMatchesEmitSeparateHunks(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line")
	}
	lines[4] = "target"
	lines[24] = "target"
	content := strings.Join(lines, "\n") + "\n"

	got := buildEditDiff(content, "target", "changed", true)
	if strings.Count(got, "@@") != 4 { // two hunks, "@@" opens and closes each header
		t.Fatalf("expected 2 hunks, got:\n%s", got)
	}
	if !strings.Contains(got, "@@ -2,7 +2,7 @@") || !strings.Contains(got, "@@ -22,7 +22,7 @@") {
		t.Errorf("unexpected hunk headers in:\n%s", got)
	}
	if strings.Count(got, "-target") != 2 || strings.Count(got, "+changed") != 2 {
		t.Errorf("expected both occurrences rendered:\n%s", got)
	}
}

func TestBuildEditDiffReplaceAllNearbyMatchesShareHunkWithInterleavedContext(t *testing.T) {
	content := "a\ntarget\nb\nc\ntarget\nd\n"
	got := buildEditDiff(content, "target", "T", true)
	want := strings.Join([]string{
		"@@ -1,6 +1,6 @@",
		" a",
		"-target",
		"+T",
		" b",
		" c",
		"-target",
		"+T",
		" d",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffReplaceAllSameLineMatchesMergeIntoOneRun(t *testing.T) {
	content := "foo bar foo\nother\n"
	got := buildEditDiff(content, "foo", "qux", true)
	want := strings.Join([]string{
		"@@ -1,2 +1,2 @@",
		"-foo bar foo",
		"+qux bar qux",
		" other",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffLineNumbersShiftAcrossHunks(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line")
	}
	lines[4] = "first"
	lines[24] = "second"
	content := strings.Join(lines, "\n") + "\n"

	// Grow the first change by two lines; the second hunk's new-file start
	// must shift by +2.
	got := buildEditDiff(strings.Replace(content, "second", "first", 1), "first", "first\nadded\nadded", true)
	if !strings.Contains(got, "@@ -2,7 +2,9 @@") {
		t.Errorf("first hunk header wrong:\n%s", got)
	}
	if !strings.Contains(got, "@@ -22,7 +24,9 @@") {
		t.Errorf("second hunk header should shift by prior delta:\n%s", got)
	}
}

func TestBuildEditDiffDeletionOfWholeLine(t *testing.T) {
	content := "a\nb\nc\n"
	got := buildEditDiff(content, "b\n", "", false)
	want := strings.Join([]string{
		"@@ -1,3 +1,2 @@",
		" a",
		"-b",
		" c",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffLineJoinWhenMatchConsumesNewline(t *testing.T) {
	// Replacing "foo\n" with "baz" joins the following line: the file
	// becomes "bazbar\n", so "bar" must not render as unchanged context.
	got := buildEditDiff("foo\nbar\n", "foo\n", "baz", false)
	want := strings.Join([]string{
		"@@ -1,2 +1,1 @@",
		"-foo",
		"-bar",
		"+bazbar",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffNoLineJoinWhenReplacementKeepsNewline(t *testing.T) {
	got := buildEditDiff("foo\nbar\n", "foo\n", "baz\n", false)
	want := strings.Join([]string{
		"@@ -1,2 +1,2 @@",
		"-foo",
		"+baz",
		" bar",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffLineJoinWithMidLinePrefix(t *testing.T) {
	// The match starts mid-line and swallows the newline: "xfoo\nbar\n"
	// becomes "xbar\n", joining the remainder of the first line with the
	// second.
	got := buildEditDiff("a\nxfoo\nbar\nc\n", "foo\n", "", false)
	want := strings.Join([]string{
		"@@ -1,4 +1,3 @@",
		" a",
		"-xfoo",
		"-bar",
		"+xbar",
		" c",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffLineJoinCascadesAcrossReplaceAll(t *testing.T) {
	// Each replacement joins the next line, so consecutive matches collapse
	// into one run: "a\na\nb\n" -> "AAb\n".
	got := buildEditDiff("a\na\nb\n", "a\n", "A", true)
	want := strings.Join([]string{
		"@@ -1,3 +1,1 @@",
		"-a",
		"-a",
		"-b",
		"+AAb",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffNoLineJoinAtEOF(t *testing.T) {
	// The consumed newline is the last byte of the file: there is no
	// following line to join, so the replacement simply becomes the new
	// final line.
	got := buildEditDiff("foo\nbar\n", "bar\n", "B", false)
	want := strings.Join([]string{
		"@@ -1,2 +1,2 @@",
		" foo",
		"-bar",
		"+B",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffAppendAtEOFWithoutTrailingNewline(t *testing.T) {
	content := "a\nb"
	got := buildEditDiff(content, "b", "b\nc", false)
	want := strings.Join([]string{
		"@@ -1,2 +1,3 @@",
		" a",
		"-b",
		"+b",
		"+c",
	}, "\n")
	if got != want {
		t.Errorf("buildEditDiff() =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildEditDiffNoMatchReturnsEmpty(t *testing.T) {
	if got := buildEditDiff("a\nb\n", "missing", "x", false); got != "" {
		t.Errorf("buildEditDiff() = %q, want empty", got)
	}
}

func TestBuildEditDiffTruncatesHugeDiffs(t *testing.T) {
	content := strings.Repeat("padding "+strings.Repeat("x", 70)+"\ntarget\n", 500)
	got := buildEditDiff(content, "target", "changed", true)
	if len(got) > maxEditDiffBytes+len(editDiffTruncationNote)+1 {
		t.Errorf("diff length = %d, want <= %d", len(got), maxEditDiffBytes+len(editDiffTruncationNote)+1)
	}
	if !strings.HasSuffix(got, editDiffTruncationNote) {
		t.Errorf("truncated diff should end with note, got tail: %q", got[len(got)-80:])
	}
}

func TestFileEditToolResultIncludesDiff(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {\n\tprintln(\"old\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&FileEditTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"file_path":  path,
		"old_string": "println(\"old\")",
		"new_string": "println(\"new\")",
	}), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() IsError, content = %q", result.Content)
	}
	if !strings.HasPrefix(result.Content, "Successfully edited "+path) {
		t.Errorf("content missing summary line: %q", result.Content)
	}
	for _, want := range []string{"@@ -1,5 +1,5 @@", "-\tprintln(\"old\")", "+\tprintln(\"new\")", " func main() {"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content missing %q:\n%s", want, result.Content)
		}
	}
}

func TestWorkspaceEditToolResultIncludesDiff(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "note.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&WorkspaceEditTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"file_path":  "note.txt",
		"old_string": "beta",
		"new_string": "BETA",
	}), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() IsError, content = %q", result.Content)
	}
	for _, want := range []string{"Successfully edited", "@@ -1,3 +1,3 @@", "-beta", "+BETA", " alpha", " gamma"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content missing %q:\n%s", want, result.Content)
		}
	}
}

func TestWorkspaceEditToolReplaceAllResultIncludesDiff(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "note.txt"), []byte("x\ny\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&WorkspaceEditTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"file_path":   "note.txt",
		"old_string":  "x",
		"new_string":  "z",
		"replace_all": true,
	}), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() IsError, content = %q", result.Content)
	}
	if !strings.Contains(result.Content, "Successfully replaced 2 occurrences") {
		t.Errorf("content missing replace summary: %q", result.Content)
	}
	if strings.Count(result.Content, "-x") != 2 || strings.Count(result.Content, "+z") != 2 {
		t.Errorf("content should show both replacements:\n%s", result.Content)
	}
}
