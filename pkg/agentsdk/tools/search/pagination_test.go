package search

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type decodedPage[T any] struct {
	Matches      T        `json:"matches"`
	Truncated    bool     `json:"truncated"`
	NextCursor   string   `json:"next_cursor"`
	Incomplete   bool     `json:"incomplete"`
	OmittedFiles []string `json:"omitted_files"`
}

func decodePage[T any](t *testing.T, content string) decodedPage[T] {
	t.Helper()
	var page decodedPage[T]
	if err := json.Unmarshal([]byte(content), &page); err != nil {
		t.Fatalf("decode page %q: %v", content, err)
	}
	return page
}

func TestGlobPaginationIsCompleteAndStable(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"c.go", "a.go", "b.go"} {
		writeTestFile(t, dir, name, "package p")
	}

	firstResult, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"*.go","limit":2,"output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	first := decodePage[[]string](t, firstResult.Content)
	if !first.Truncated || first.NextCursor == "" || !reflect.DeepEqual(first.Matches, []string{"a.go", "b.go"}) {
		t.Fatalf("first page = %#v", first)
	}

	input := fmt.Sprintf(`{"pattern":"*.go","limit":2,"output_format":"json","cursor":%q}`, first.NextCursor)
	secondResult, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(input), dir)
	if err != nil {
		t.Fatal(err)
	}
	second := decodePage[[]string](t, secondResult.Content)
	if second.Truncated || second.NextCursor != "" || !reflect.DeepEqual(second.Matches, []string{"c.go"}) {
		t.Fatalf("second page = %#v", second)
	}
}

func TestGlobLegacyTextSignalsTruncation(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.go", "package p")
	writeTestFile(t, dir, "b.go", "package p")

	result, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"*.go","limit":1}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"truncated":true`) || !strings.Contains(result.Content, `"next_cursor"`) {
		t.Fatalf("Content = %q, want explicit truncation metadata", result.Content)
	}
}

func TestGlobCursorIsBoundToQuery(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.go", "package p")
	writeTestFile(t, dir, "b.go", "package p")
	result, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"*.go","limit":1,"output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	page := decodePage[[]string](t, result.Content)

	input := fmt.Sprintf(`{"pattern":"*.txt","cursor":%q}`, page.NextCursor)
	mismatch, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(input), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !mismatch.IsError || !strings.Contains(mismatch.Content, "does not match") {
		t.Fatalf("result = %#v, want cursor mismatch", mismatch)
	}
}

func TestGrepPaginationContextAndStructuredMatches(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "before\nneedle one\nafter\nneedle two\nend\n")

	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","limit":1,"before_context":1,"after_context":1,"output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	page := decodePage[[]grepMatch](t, result.Content)
	if !page.Truncated || len(page.Matches) != 1 {
		t.Fatalf("page = %#v", page)
	}
	match := page.Matches[0]
	if match.Path != "a.txt" || match.Line != 2 || !reflect.DeepEqual(match.Before, []string{"before"}) || !reflect.DeepEqual(match.After, []string{"after"}) {
		t.Fatalf("match = %#v", match)
	}
}

func TestGrepFilesAndCountModes(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "needle\nneedle\n")
	writeTestFile(t, dir, "b.txt", "needle\n")

	filesResult, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","mode":"files","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	files := decodePage[[]string](t, filesResult.Content)
	if !reflect.DeepEqual(files.Matches, []string{"a.txt", "b.txt"}) {
		t.Fatalf("files = %#v", files.Matches)
	}

	countResult, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","mode":"count","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	counts := decodePage[[]grepCount](t, countResult.Content)
	if !reflect.DeepEqual(counts.Matches, []grepCount{{Path: "a.txt", Count: 2}, {Path: "b.txt", Count: 1}}) {
		t.Fatalf("counts = %#v", counts.Matches)
	}
}

func TestSearchIncludeExcludeAndGitignoreControls(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".gitignore", "ignored.txt\n")
	writeTestFile(t, dir, "keep.go", "needle")
	writeTestFile(t, dir, "skip_test.go", "needle")
	writeTestFile(t, dir, "ignored.txt", "needle")
	writeTestFile(t, dir, "visible.txt", "needle")

	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","include":["*.go","*.txt"],"exclude":["*_test.go"],"respect_gitignore":true,"mode":"files","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	page := decodePage[[]string](t, result.Content)
	if !reflect.DeepEqual(page.Matches, []string{"keep.go", "visible.txt"}) {
		t.Fatalf("matches = %#v", page.Matches)
	}
}

func TestDefaultDirectorySkippingCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, filepath.Join("vendor", "dep.go"), "needle")

	defaultResult, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","mode":"files","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if page := decodePage[[]string](t, defaultResult.Content); len(page.Matches) != 0 {
		t.Fatalf("default matches = %#v, want none", page.Matches)
	}

	allResult, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","mode":"files","skip_default_dirs":false,"output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if page := decodePage[[]string](t, allResult.Content); !reflect.DeepEqual(page.Matches, []string{"vendor/dep.go"}) {
		t.Fatalf("all matches = %#v", page.Matches)
	}
}

func TestGrepPaginationDoesNotDuplicateOrSkip(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "needle one\nneedle two\nneedle three\n")

	firstResult, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","limit":2,"output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	first := decodePage[[]grepMatch](t, firstResult.Content)
	input := fmt.Sprintf(`{"pattern":"needle","limit":2,"output_format":"json","cursor":%q}`, first.NextCursor)
	secondResult, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(input), dir)
	if err != nil {
		t.Fatal(err)
	}
	second := decodePage[[]grepMatch](t, secondResult.Content)
	if got := []int{first.Matches[0].Line, first.Matches[1].Line, second.Matches[0].Line}; !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("line sequence = %#v", got)
	}
	if second.Truncated {
		t.Fatal("final page unexpectedly truncated")
	}
}

func TestStructuredEmptyMatchesIsArray(t *testing.T) {
	dir := t.TempDir()
	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"absent","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"matches":[]`) {
		t.Fatalf("Content = %q, want empty matches array", result.Content)
	}
}

func TestStructuredGrepReportsOmittedLongLine(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "long.txt", "needle first\n"+strings.Repeat("x", 2*1024*1024)+"\n")
	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	page := decodePage[[]grepMatch](t, result.Content)
	if len(page.Matches) != 1 || !page.Incomplete || len(page.OmittedFiles) != 1 {
		t.Fatalf("page = %#v", page)
	}
}

func TestPathFiltersSupportCharacterClassesAndZeroDirectoryDoubleStar(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, filepath.Join("src", "a.go"), "needle")
	writeTestFile(t, dir, filepath.Join("src", "nested", "x.go"), "needle")
	writeTestFile(t, dir, filepath.Join("src", "x.go"), "needle")

	classResult, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.go","include":["src/[ab].go"],"output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if page := decodePage[[]string](t, classResult.Content); !reflect.DeepEqual(page.Matches, []string{"src/a.go"}) {
		t.Fatalf("class matches = %#v", page.Matches)
	}
	doubleResult, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"src/**/x.go","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if page := decodePage[[]string](t, doubleResult.Content); !reflect.DeepEqual(page.Matches, []string{"src/nested/x.go", "src/x.go"}) {
		t.Fatalf("doublestar matches = %#v", page.Matches)
	}
}

func TestGitignorePathRulesApplyToDescendants(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".gitignore", "/build\ncache/\n")
	writeTestFile(t, dir, filepath.Join("build", "out.txt"), "needle")
	writeTestFile(t, dir, filepath.Join("cache", "nested.txt"), "needle")
	writeTestFile(t, dir, filepath.Join("sub", "cache"), "needle")
	writeTestFile(t, dir, "visible.txt", "needle")

	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","respect_gitignore":true,"mode":"files","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	page := decodePage[[]string](t, result.Content)
	if !reflect.DeepEqual(page.Matches, []string{"sub/cache", "visible.txt"}) {
		t.Fatalf("matches = %#v", page.Matches)
	}
}

func TestGitignoreSymlinkIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-ignore")
	writeTestFile(t, root, "outside-ignore", "secret.txt\n")
	writeTestFile(t, dir, "secret.txt", "needle")
	if err := os.Symlink(outside, filepath.Join(dir, ".gitignore")); err != nil {
		t.Skip(err)
	}

	result, err := (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","respect_gitignore":true,"mode":"files","output_format":"json"}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if page := decodePage[[]string](t, result.Content); !reflect.DeepEqual(page.Matches, []string{"secret.txt"}) {
		t.Fatalf("matches = %#v", page.Matches)
	}
}

func TestSearchRejectsUnboundedPageAndContext(t *testing.T) {
	dir := t.TempDir()
	result, err := (&GlobTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"*","limit":1001}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "must not exceed") {
		t.Fatalf("result = %#v", result)
	}
	result, err = (&GrepTool{}).Execute(context.Background(), json.RawMessage(`{"pattern":"x","before_context":11}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "must not exceed") {
		t.Fatalf("result = %#v", result)
	}
}
