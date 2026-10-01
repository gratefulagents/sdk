package projectstatetools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/projectstate"
)

var _ StalenessChecker = (*GitStalenessChecker)(nil)

func TestToolNames(t *testing.T) {
	var names []string
	for _, tool := range Tools(newTestStore(t), "assistant") {
		names = append(names, tool.Name())
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
			t.Fatalf("%s schema: %v", tool.Name(), err)
		}
	}
	want := "task_create task_ready task_show task_update task_claim task_close task_comment task_link memory_search memory_get memory_save memory_verify memory_delete prime_context"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("tool names = %s\nwant %s", got, want)
	}
}

func TestMemoryToolsLifecycle(t *testing.T) {
	store := newTestStore(t)
	tools := toolsByName(Tools(store, "assistant"))

	saved := execTool(t, tools["memory_save"], `{
		"kind": "preference",
		"title": "Concise answers",
		"body": "The user prefers concise answers without preamble.",
		"citations": [{"path": "docs/style.md"}, {"url": "https://example.test/style"}]
	}`)
	var saveOut struct {
		Memory  projectstate.Memory `json:"memory"`
		Warning string              `json:"warning"`
	}
	if err := json.Unmarshal([]byte(saved.Content), &saveOut); err != nil {
		t.Fatal(err)
	}
	mem := saveOut.Memory
	if mem.ID == "" || mem.Kind != projectstate.MemoryKindPreference || len(mem.Citations) != 2 || mem.SourceRun != "assistant" || saveOut.Warning != "" {
		t.Fatalf("saved = %s", saved.Content)
	}

	searched := execTool(t, tools["memory_search"], `{"query":"concise answers"}`)
	var hits []memorySearchHit
	if err := json.Unmarshal([]byte(searched.Content), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != mem.ID || hits[0].Score != 1 || hits[0].Snippet == "" || hits[0].Stale {
		t.Fatalf("search = %s", searched.Content)
	}
	if out := execTool(t, tools["memory_search"], `{"query":"concise","kinds":["fact"]}`); strings.TrimSpace(out.Content) != "[]" {
		t.Fatalf("kind-filtered search = %s", out.Content)
	}

	got := execTool(t, tools["memory_get"], `{"id":"`+mem.ID+`"}`)
	var getOut memoryWithStaleness
	if err := json.Unmarshal([]byte(got.Content), &getOut); err != nil {
		t.Fatal(err)
	}
	if getOut.Body != mem.Body || getOut.Stale {
		t.Fatalf("get = %s", got.Content)
	}
	touched, err := store.GetMemory(t.Context(), mem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if touched.UseCount != 1 || touched.LastUsedAt == nil {
		t.Fatalf("memory_get did not touch: %+v", touched)
	}

	// Update by id keeps omitted fields.
	updated := execTool(t, tools["memory_save"], `{"id":"`+mem.ID+`","body":"The user prefers terse answers."}`)
	if err := json.Unmarshal([]byte(updated.Content), &saveOut); err != nil {
		t.Fatal(err)
	}
	if saveOut.Memory.ID != mem.ID || saveOut.Memory.Title != "Concise answers" || saveOut.Memory.Kind != projectstate.MemoryKindPreference ||
		len(saveOut.Memory.Citations) != 2 || saveOut.Memory.Body != "The user prefers terse answers." || saveOut.Memory.UseCount != 1 {
		t.Fatalf("updated = %s", updated.Content)
	}
	missing, err := tools["memory_save"].Execute(t.Context(), json.RawMessage(`{"id":"mem_missing","title":"x","body":"y"}`), "")
	if err != nil || !missing.IsError || !strings.Contains(missing.Content, "not found") {
		t.Fatalf("save unknown id = %+v err=%v", missing, err)
	}

	verified := execTool(t, tools["memory_verify"], `{"id":"`+mem.ID+`"}`)
	if !strings.Contains(verified.Content, mem.ID) {
		t.Fatalf("verify = %s", verified.Content)
	}

	deleted := execTool(t, tools["memory_delete"], `{"id":"`+mem.ID+`","reason":"superseded"}`)
	if !strings.Contains(deleted.Content, `"deleted"`) || !strings.Contains(deleted.Content, "superseded") {
		t.Fatalf("delete result = %s", deleted.Content)
	}
	memories, err := store.ListMemories(t.Context(), projectstate.MemoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 0 {
		t.Fatalf("memories after delete = %#v, want empty", memories)
	}
}

func TestMemorySaveRejectsDuplicates(t *testing.T) {
	store := newTestStore(t)
	tools := toolsByName(Tools(store, "assistant"))
	first := execTool(t, tools["memory_save"], `{"kind":"fact","title":"Integration tests need Docker","body":"The integration test suite starts Postgres in Docker; run it with make test-integration."}`)
	var out struct {
		Memory projectstate.Memory `json:"memory"`
	}
	if err := json.Unmarshal([]byte(first.Content), &out); err != nil {
		t.Fatal(err)
	}

	dupInput := `{"kind":"fact","title":"Integration tests require Docker","body":"The integration test suite starts Postgres in Docker; run it via make test-integration."}`
	result, err := tools["memory_save"].Execute(t.Context(), json.RawMessage(dupInput), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, out.Memory.ID) || !strings.Contains(result.Content, "Integration tests need Docker") || !strings.Contains(result.Content, "memory_save with its id") {
		t.Fatalf("duplicate save = %+v, want rejection listing %s", result, out.Memory.ID)
	}
	if list, _ := store.ListMemories(t.Context(), projectstate.MemoryFilter{}); len(list) != 1 {
		t.Fatalf("memories after rejected duplicate = %d, want 1", len(list))
	}

	forced := strings.Replace(dupInput, `"kind"`, `"allow_duplicate":true,"kind"`, 1)
	execTool(t, tools["memory_save"], forced)
	execTool(t, tools["memory_save"], `{"kind":"preference","title":"Use tabs","body":"Indent Go with gofmt defaults."}`)
	if list, _ := store.ListMemories(t.Context(), projectstate.MemoryFilter{}); len(list) != 3 {
		t.Fatalf("memories = %d, want 3", len(list))
	}
}

func TestMemorySaveWarnsOnProgressLog(t *testing.T) {
	tools := toolsByName(Tools(newTestStore(t), "assistant"))
	result := execTool(t, tools["memory_save"], `{"kind":"fact","title":"Auth refactor done","body":"Merged PR #12 and CI is green."}`)
	if !strings.Contains(result.Content, `"warning"`) {
		t.Fatalf("expected progress-log warning, got %s", result.Content)
	}
}

func TestGitStalenessChecker(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.test", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a.go", "package a\n")
	write("docs/b.md", "b\n")
	git("add", ".")
	git("commit", "-q", "-m", "init")

	checker := NewGitStalenessChecker(dir)
	head := checker.HeadSHA(ctx)
	if len(head) != 40 {
		t.Fatalf("HeadSHA = %q", head)
	}
	now := time.Now()
	mem := projectstate.Memory{ID: "mem_1", CommitSHA: head, VerifiedAt: now, Citations: []projectstate.Citation{{Path: "a.go"}, {Path: "docs/b.md"}, {URL: "https://example.test"}}}

	if stale, reason := checker.Check(ctx, mem); stale {
		t.Fatalf("fresh memory stale: %s", reason)
	}

	// Uncommitted edits do not make a memory stale: save/verify stamp HEAD,
	// so only committed history after the stamp counts.
	write("a.go", "package a\n\nconst X = 1\n")
	write("docs/b.md", "changed\n")
	if stale, reason := checker.Check(ctx, mem); stale {
		t.Fatalf("uncommitted edits marked stale: %s", reason)
	}
	git("commit", "-q", "-am", "change cited files")
	stale, reason := checker.Check(ctx, mem)
	if !stale || reason != "cited files changed since verification: a.go, docs/b.md" {
		t.Fatalf("changed files: stale=%v reason=%q", stale, reason)
	}

	if err := os.Remove(filepath.Join(dir, "docs/b.md")); err != nil {
		t.Fatal(err)
	}
	if stale, reason := checker.Check(ctx, mem); !stale || reason != "cited file docs/b.md no longer exists" {
		t.Fatalf("missing file: stale=%v reason=%q", stale, reason)
	}

	unknown := mem
	unknown.Citations = []projectstate.Citation{{Path: "a.go"}}
	unknown.CommitSHA = "0123456789abcdef0123456789abcdef01234567"
	if stale, reason := checker.Check(ctx, unknown); !stale || reason != "verification commit not found" {
		t.Fatalf("unknown sha: stale=%v reason=%q", stale, reason)
	}

	old := projectstate.Memory{ID: "mem_2", VerifiedAt: now.Add(-100 * 24 * time.Hour)}
	if stale, reason := checker.Check(ctx, old); !stale || !strings.HasPrefix(reason, "not verified for 100 days") {
		t.Fatalf("old memory: stale=%v reason=%q", stale, reason)
	}

	// Without a workdir only the age rule applies; git errors never mark stale.
	noDir := NewGitStalenessChecker("")
	if stale, _ := noDir.Check(ctx, unknown); stale {
		t.Fatal("no-workdir checker should ignore citations")
	}
	if noDir.HeadSHA(ctx) != "" {
		t.Fatal("no-workdir HeadSHA should be empty")
	}
	notRepo := NewGitStalenessChecker(t.TempDir())
	if err := os.WriteFile(filepath.Join(notRepo.dir, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stale, reason := notRepo.Check(ctx, unknown); stale {
		t.Fatalf("non-repo dir should not be stale, got %q", reason)
	}

	// Tools record HEAD on save and surface staleness on get once a later
	// commit changes a cited file.
	store := newTestStore(t)
	tools := toolsByName(Tools(store, "assistant", WithWorkDir(dir)))
	write("docs/b.md", "restored\n")
	git("add", ".")
	git("commit", "-q", "-m", "restore b")
	head = checker.HeadSHA(ctx)
	saved := execTool(t, tools["memory_save"], `{"kind":"fact","title":"Package a","body":"Package a defines X.","citations":[{"path":"a.go"}]}`)
	var out struct {
		Memory projectstate.Memory `json:"memory"`
	}
	if err := json.Unmarshal([]byte(saved.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Memory.CommitSHA != head {
		t.Fatalf("saved CommitSHA = %q, want HEAD %q", out.Memory.CommitSHA, head)
	}
	if got := execTool(t, tools["memory_get"], `{"id":"`+out.Memory.ID+`"}`); strings.Contains(got.Content, `"stale": true`) {
		t.Fatalf("fresh memory reported stale: %s", got.Content)
	}
	write("a.go", "package a\n\nconst Y = 2\n")
	git("commit", "-q", "-am", "change a")
	got := execTool(t, tools["memory_get"], `{"id":"`+out.Memory.ID+`"}`)
	if !strings.Contains(got.Content, `"stale": true`) || !strings.Contains(got.Content, "a.go") {
		t.Fatalf("memory_get should report the committed change to a.go as stale: %s", got.Content)
	}
	verified := execTool(t, tools["memory_verify"], `{"id":"`+out.Memory.ID+`"}`)
	if strings.Contains(verified.Content, `"stale": true`) {
		t.Fatalf("memory_verify should clear commit staleness: %s", verified.Content)
	}
}

func newTestStore(t *testing.T) projectstate.Store {
	t.Helper()
	store, err := projectstate.NewFilesystemStore(projectstate.FilesystemOptions{
		StateDir:  t.TempDir(),
		ProjectID: "test-project",
		Actor:     "assistant",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func toolsByName(tools []agentsdk.Tool) map[string]agentsdk.Tool {
	out := map[string]agentsdk.Tool{}
	for _, tool := range tools {
		out[tool.Name()] = tool
	}
	return out
}

func execTool(t *testing.T, tool agentsdk.Tool, input string) agentsdk.ToolResult {
	t.Helper()
	result, err := tool.Execute(t.Context(), json.RawMessage(input), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s returned error: %s", tool.Name(), result.Content)
	}
	return result
}
