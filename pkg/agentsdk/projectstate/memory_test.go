package projectstate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	_ Store = (*FilesystemStore)(nil)
	_ Store = (*SQLiteStore)(nil)
)

func TestNormalizeMemoryKind(t *testing.T) {
	cases := map[string]string{
		"preference":   MemoryKindPreference,
		" Decision ":   MemoryKindDecision,
		"FACT":         MemoryKindFact,
		"procedure":    MemoryKindProcedure,
		"pinned":       MemoryKindDecision,
		"semantic":     MemoryKindFact,
		"episodic":     MemoryKindFact,
		"procedural":   MemoryKindProcedure,
		"":             MemoryKindFact,
		"something":    MemoryKindFact,
		" PROCEDURAL ": MemoryKindProcedure,
	}
	for in, want := range cases {
		if got := NormalizeMemoryKind(in); got != want {
			t.Errorf("NormalizeMemoryKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLegacyMemoryTitle(t *testing.T) {
	cases := map[string]string{
		"Use append-only state. It survives crashes.": "Use append-only state.",
		"  first   line\nsecond line":                 "first line",
		"Single sentence.":                            "Single sentence.",
	}
	for in, want := range cases {
		if got := LegacyMemoryTitle(in); got != want {
			t.Errorf("LegacyMemoryTitle(%q) = %q, want %q", in, got, want)
		}
	}
	long := LegacyMemoryTitle(strings.Repeat("é", 150))
	if n := len([]rune(long)); n != 100 || !strings.HasSuffix(long, "...") {
		t.Fatalf("long title = %q (%d runes), want 100 runes ending in ...", long, n)
	}
}

func TestValidateMemoryInput(t *testing.T) {
	in := SaveMemoryInput{
		ID:    "  mem_1 ",
		Kind:  " Pinned ",
		Title: "  Use   pnpm \n not npm ",
		Body:  "\n The lockfile is pnpm-lock.yaml.\n\nKeep it. ",
		Citations: []Citation{
			{Path: " ./package.json "},
			{},
			{Path: "package.json"},
			{URL: " https://example.test/doc "},
		},
	}
	if err := ValidateMemoryInput(&in); err != nil {
		t.Fatal(err)
	}
	want := SaveMemoryInput{
		ID:        "mem_1",
		Kind:      MemoryKindDecision,
		Title:     "Use pnpm not npm",
		Body:      "The lockfile is pnpm-lock.yaml.\n\nKeep it.",
		Citations: []Citation{{Path: "package.json"}, {URL: "https://example.test/doc"}},
	}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("normalized = %#v\nwant %#v", in, want)
	}

	errCases := []struct {
		name string
		in   SaveMemoryInput
		want string
	}{
		{"missing title", SaveMemoryInput{Body: "b"}, "title is required"},
		{"missing body", SaveMemoryInput{Title: "t"}, "body is required"},
		{"long title", SaveMemoryInput{Title: strings.Repeat("x", MaxMemoryTitleLen+1), Body: "b"}, "Shorten"},
		{"long body", SaveMemoryInput{Title: "t", Body: strings.Repeat("x", MaxMemoryBodyLen+1)}, "split"},
		{"absolute path", SaveMemoryInput{Title: "t", Body: "b", Citations: []Citation{{Path: "/etc/passwd"}}}, "workspace-relative"},
		{"dotdot path", SaveMemoryInput{Title: "t", Body: "b", Citations: []Citation{{Path: "a/../../b"}}}, ".."},
	}
	for _, tc := range errCases {
		err := ValidateMemoryInput(&tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
	exact := SaveMemoryInput{Title: strings.Repeat("é", MaxMemoryTitleLen), Body: strings.Repeat("é", MaxMemoryBodyLen)}
	if err := ValidateMemoryInput(&exact); err != nil {
		t.Fatalf("limits are in runes, got %v", err)
	}
}

func TestProgressLogWarning(t *testing.T) {
	logs := [][2]string{
		{"Shipped auth refactor", "Merged PR #412 after CI went green."},
		{"Progress", "Commit 3f9a2c1d landed; all tests passed."},
		{"Done", "See https://github.com/acme/repo/pull/7 — merged."},
	}
	for _, tc := range logs {
		if ProgressLogWarning(tc[0], tc[1]) == "" {
			t.Errorf("ProgressLogWarning(%q, %q) = \"\", want warning", tc[0], tc[1])
		}
	}
	knowledge := [][2]string{
		{"Use pnpm, not npm", "The repo pins pnpm via packageManager; npm install corrupts the lockfile."},
		{"Merge strategy", "Release branches are merged with --no-ff so the release commit stays visible."},
		{"Decade cache", "Cache keys use the facade id, never the raw deadbeef sentinel."},
	}
	for _, tc := range knowledge {
		if got := ProgressLogWarning(tc[0], tc[1]); got != "" {
			t.Errorf("ProgressLogWarning(%q, %q) = %q, want none", tc[0], tc[1], got)
		}
	}
}

func TestTokenize(t *testing.T) {
	got := Tokenize("The foo-bar_baz API, v2 is in a FOO state!")
	want := []string{"foo-bar_baz", "foo", "bar", "baz", "api", "v2", "state"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %q, want %q", got, want)
	}
	if got := Tokenize("a I the -- _"); len(got) != 0 {
		t.Fatalf("Tokenize of stopwords/short = %q, want empty", got)
	}
}

func TestLexicalScoreRanking(t *testing.T) {
	title := Memory{Title: "Release checklist", Body: "Steps to cut a version."}
	body := Memory{Title: "Shipping", Body: "Follow the release checklist in docs."}
	cited := Memory{Title: "Shipping", Body: "See doc.", Citations: []Citation{{Path: "docs/release.md"}}}
	unrelated := Memory{Title: "Editor", Body: "The user prefers vim."}

	q := "release checklist"
	ts, bs, cs, us := LexicalScore(q, title), LexicalScore(q, body), LexicalScore(q, cited), LexicalScore(q, unrelated)
	if ts != 1 {
		t.Fatalf("title score = %v, want 1", ts)
	}
	if math.Abs(bs-0.6) > 1e-9 {
		t.Fatalf("body score = %v, want 0.6", bs)
	}
	if math.Abs(cs-0.3) > 1e-9 {
		t.Fatalf("citation score = %v, want 0.3 (one of two terms)", cs)
	}
	if us != 0 {
		t.Fatalf("unrelated score = %v, want 0", us)
	}
	if !(ts > bs && bs > cs && cs > us) {
		t.Fatalf("ranking title=%v body=%v cited=%v unrelated=%v", ts, bs, cs, us)
	}
	if LexicalScore("the and", title) != 0 {
		t.Fatal("stopword-only query should score 0")
	}
}

func TestSimilarity(t *testing.T) {
	if got := Similarity("Use pnpm", "not npm", "use PNPM", "not NPM!"); got != 1 {
		t.Fatalf("identical similarity = %v, want 1", got)
	}
	if got := Similarity("alpha", "beta", "gamma", "delta"); got != 0 {
		t.Fatalf("disjoint similarity = %v, want 0", got)
	}
	got := Similarity("deploy friday", "deploys happen on friday", "deploy monday", "deploys happen on monday")
	if got <= 0 || got >= 1 {
		t.Fatalf("partial similarity = %v, want in (0,1)", got)
	}
	if Similarity("", "", "x", "y") != 0 {
		t.Fatal("empty similarity should be 0")
	}
}

func TestSelectBriefingMemoriesOrderBudgetAndDeterminism(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	used := base.Add(48 * time.Hour)
	all := []Memory{
		{ID: "mem_fact_old", Kind: MemoryKindFact, Title: "old fact", VerifiedAt: base.Add(-60 * 24 * time.Hour)},
		{ID: "mem_pref", Kind: MemoryKindPreference, Title: "pref", UpdatedAt: base},
		{ID: "mem_dec_new", Kind: MemoryKindDecision, Title: "new decision", UpdatedAt: base.Add(time.Hour)},
		{ID: "mem_fact_used", Kind: MemoryKindFact, Title: "used fact", VerifiedAt: base, UseCount: 5, LastUsedAt: &used},
		{ID: "mem_proc", Kind: MemoryKindProcedure, Title: "fresh procedure", VerifiedAt: base.Add(24 * time.Hour)},
		{ID: "mem_b", Kind: MemoryKindFact, Title: "tie b", VerifiedAt: base},
		{ID: "mem_a", Kind: MemoryKindFact, Title: "tie a", VerifiedAt: base},
	}
	got := SelectBriefingMemories(all, 0)
	ids := make([]string, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	want := []string{"mem_dec_new", "mem_pref", "mem_fact_used", "mem_proc", "mem_a", "mem_b", "mem_fact_old"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		shuffled := append([]Memory(nil), all...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if again := SelectBriefingMemories(shuffled, 0); !reflect.DeepEqual(again, got) {
			t.Fatalf("selection depends on input order")
		}
	}

	budget := len(memoryIndexLine(all[2])) + len(memoryIndexLine(all[1]))
	small := SelectBriefingMemories(all, budget)
	if len(small) != 2 || small[0].ID != "mem_dec_new" || small[1].ID != "mem_pref" {
		t.Fatalf("budgeted selection = %+v", small)
	}
	if got := SelectBriefingMemories(all, budget-1); len(got) != 1 {
		t.Fatalf("budget-1 selection = %d memories, want 1", len(got))
	}

	var many []Memory
	for i := 0; i < 100; i++ {
		many = append(many, Memory{ID: fmt.Sprintf("mem_%03d", i), Kind: MemoryKindFact, Title: strings.Repeat("t", 80)})
	}
	total := 0
	for _, m := range SelectBriefingMemories(many, 0) {
		total += len(memoryIndexLine(m))
	}
	if total > BriefingMemoryBudget || total == 0 {
		t.Fatalf("rendered bytes = %d, want within (0, %d]", total, BriefingMemoryBudget)
	}
}

func TestRenderBriefing(t *testing.T) {
	got := RenderBriefing(BriefingInput{
		ProjectID: "proj",
		Active:    &Task{ID: "task_a", Title: "Ship v2", Status: TaskStatusInProgress, Priority: 1, Description: "Finish   memory v2.\nThen docs."},
		Ready:     []Task{{ID: "task_b", Title: "Write docs", Status: TaskStatusOpen, Priority: 2}},
		Blocked:   []Task{{ID: "task_c", Title: "Release", Status: TaskStatusOpen, Priority: 3, DependsOn: []string{"task_b"}}},
		Memories: []Memory{
			{ID: "mem_2", Kind: MemoryKindFact, Title: "Lock file lives in locks/", VerifiedAt: time.Unix(100, 0)},
			{ID: "mem_1", Kind: MemoryKindDecision, Title: "Use events", UpdatedAt: time.Unix(50, 0)},
		},
	})
	want := `## Durable Project State
Project: proj

### Active Task
- task_a [P1 in_progress] Ship v2
  Finish memory v2. Then docs.

### Ready Work
- task_b [P2 open] Write docs

### Blocked Work
- task_c [P3 open] Release

### Memory Index (2 of 2) — memory_get <id> for full text; memory_search for anything not listed
- mem_1 [decision] Use events
- mem_2 [fact] Lock file lives in locks/`
	if got != want {
		t.Fatalf("briefing =\n%s\nwant\n%s", got, want)
	}

	empty := RenderBriefing(BriefingInput{ProjectID: "proj"})
	if empty != "## Durable Project State\nProject: proj\nNo durable tasks or memories yet." {
		t.Fatalf("empty briefing = %q", empty)
	}
}

func TestVectorLiteral(t *testing.T) {
	if got := VectorLiteral(nil); got != "[]" {
		t.Fatalf("VectorLiteral(nil) = %q", got)
	}
	if got := VectorLiteral([]float32{0.5, -1, 0}); got != "[0.5,-1,0]" {
		t.Fatalf("VectorLiteral = %q", got)
	}
}

func TestTaskHelpers(t *testing.T) {
	now := time.Unix(1000, 0)
	tasks := []Task{
		{ID: "task_dep", Status: TaskStatusOpen, Priority: 1, UpdatedAt: now},
		{ID: "task_blocked", Status: TaskStatusOpen, Priority: 0, DependsOn: []string{"task_dep"}, UpdatedAt: now},
		{ID: "task_mine", Status: TaskStatusOpen, Priority: 2, Assignee: "alice", UpdatedAt: now},
		{ID: "task_theirs", Status: TaskStatusOpen, Priority: 2, Assignee: "bob", UpdatedAt: now},
		{ID: "task_wip_b", Status: TaskStatusInProgress, Assignee: "alice", Priority: 2, UpdatedAt: now},
		{ID: "task_wip_a", Status: TaskStatusInProgress, Assignee: "alice", Priority: 2, UpdatedAt: now},
	}
	ready := ReadyFromTasks(tasks, TaskFilter{Actor: "alice"})
	if ids := taskIDs(ready); !reflect.DeepEqual(ids, []string{"task_dep", "task_mine"}) {
		t.Fatalf("ready = %v", ids)
	}
	if ids := taskIDs(ReadyFromTasks(tasks, TaskFilter{IncludeAssigned: true, Limit: 2})); len(ids) != 2 {
		t.Fatalf("limited ready = %v", ids)
	}
	if ids := taskIDs(BlockedFromTasks(tasks, 5)); !reflect.DeepEqual(ids, []string{"task_blocked"}) {
		t.Fatalf("blocked = %v", ids)
	}
	if active := ActiveTask(tasks, "", "alice"); active == nil || active.ID != "task_wip_a" {
		t.Fatalf("active = %+v, want deterministic task_wip_a", active)
	}
	if active := ActiveTask(tasks, "task_mine", "alice"); active == nil || active.ID != "task_mine" {
		t.Fatalf("explicit active = %+v", active)
	}
	if ActiveTask(tasks, "", "carol") != nil {
		t.Fatal("carol has no active task")
	}
	byID := tasksByID(tasks)
	RecomputeBlocks(byID)
	if got := byID["task_dep"].Blocks; !reflect.DeepEqual(got, []string{"task_blocked"}) {
		t.Fatalf("blocks = %v", got)
	}
	task := Task{Status: TaskStatusOpen}
	status, prio := "done", 9
	ApplyTaskPatch(&task, TaskPatch{Status: &status, Priority: &prio}, now)
	if task.Status != TaskStatusClosed || task.ClosedAt == nil || task.Priority != 4 || !task.UpdatedAt.Equal(now) {
		t.Fatalf("patched task = %+v", task)
	}
}

func taskIDs(tasks []Task) []string {
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
	}
	return ids
}

func TestLegacyUpsertedEventReplay(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := NewFilesystemStore(FilesystemOptions{StateDir: dir, ProjectID: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	legacy := []Event{
		{Seq: 50, EventID: "evt_1", ProjectID: "legacy", Type: "memory.upserted", Time: updated, Payload: mustJSON(t, map[string]any{
			"id": "mem_old", "kind": "pinned", "scope": "project",
			"content":    "Always run make lint before pushing. CI rejects unformatted code.\nMore details here.",
			"tags":       []string{"ci"},
			"file_paths": []string{"Makefile", "", "Makefile"},
			"source_run": "run-1", "created_at": updated.Add(-time.Hour), "updated_at": updated,
		})},
		{Seq: 51, EventID: "evt_2", ProjectID: "legacy", Type: "memory.upserted", Time: updated, Payload: mustJSON(t, map[string]any{
			"id": "mem_sem", "kind": "episodic", "content": "Deploys happen on Fridays", "updated_at": updated,
		})},
		{Seq: 52, EventID: "evt_3", ProjectID: "legacy", Type: "session.summary_saved", Time: updated, Payload: mustJSON(t, map[string]any{
			"id": "session_1", "summary": "did things",
		})},
	}
	f, err := os.OpenFile(filepath.Join(dir, eventsFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range legacy {
		line, _ := json.Marshal(ev)
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	_ = store.Close()

	reopened, err := NewFilesystemStore(FilesystemOptions{StateDir: dir, ProjectID: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := reopened.GetMemory(ctx, "mem_old")
	if err != nil {
		t.Fatal(err)
	}
	if mem.Kind != MemoryKindDecision || mem.Title != "Always run make lint before pushing." ||
		!strings.HasPrefix(mem.Body, "Always run make lint") || !strings.Contains(mem.Body, "More details here.") ||
		!reflect.DeepEqual(mem.Citations, []Citation{{Path: "Makefile"}}) || !mem.VerifiedAt.Equal(updated) ||
		mem.SourceRun != "run-1" {
		t.Fatalf("legacy memory = %+v", mem)
	}
	other, err := reopened.GetMemory(ctx, "mem_sem")
	if err != nil {
		t.Fatal(err)
	}
	if other.Kind != MemoryKindFact || other.Title != "Deploys happen on Fridays" {
		t.Fatalf("legacy episodic memory = %+v", other)
	}
	// New writes continue after the legacy seq and the legacy memory stays
	// updatable through the v2 API.
	if _, err := reopened.SaveMemory(ctx, SaveMemoryInput{ID: "mem_old", Kind: MemoryKindProcedure, Title: "Lint before push", Body: "Run make lint."}); err != nil {
		t.Fatal(err)
	}
	prime, err := reopened.PrimeContext(ctx, PrimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prime, "- mem_old [procedure] Lint before push") || !strings.Contains(prime, "(2 of 2)") {
		t.Fatalf("prime after legacy replay:\n%s", prime)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReleaseClaims(t *testing.T) {
	for name, opener := range storeOpeners() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			open := opener(t)
			store := open()
			var ids []string
			for _, title := range []string{"one", "two", "three"} {
				task, err := store.CreateTask(ctx, CreateTaskInput{Title: title})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, task.ID)
			}
			for i, actor := range []string{"alice", "alice", "bob"} {
				if _, err := store.ClaimTask(ctx, ids[i], actor); err != nil {
					t.Fatal(err)
				}
			}
			released, err := store.ReleaseClaims(ctx, "alice", "run ended")
			if err != nil {
				t.Fatal(err)
			}
			if len(released) != 2 {
				t.Fatalf("released = %+v, want 2 tasks", released)
			}
			check := func(s Store) {
				for i, id := range ids {
					task, err := s.GetTask(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if i < 2 {
						if task.Status != TaskStatusOpen || task.Assignee != "" || len(task.Comments) != 1 || task.Comments[0].Body != "run ended" {
							t.Fatalf("released task = %+v", task)
						}
					} else if task.Status != TaskStatusInProgress || task.Assignee != "bob" {
						t.Fatalf("bob's task = %+v", task)
					}
				}
			}
			check(store)
			again, err := store.ReleaseClaims(ctx, "alice", "run ended")
			if err != nil || len(again) != 0 {
				t.Fatalf("second release = %+v err=%v, want none", again, err)
			}
			if _, err := store.ReleaseClaims(ctx, " ", ""); err == nil {
				t.Fatal("empty actor should error")
			}
			check(open())
		})
	}
}

func TestMemoryReplayAcrossReopen(t *testing.T) {
	for name, opener := range storeOpeners() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			open := opener(t)
			store := open()
			mem, err := store.SaveMemory(ctx, SaveMemoryInput{Kind: "fact", Title: "Lock path", Body: "The state lock lives in locks/state.lock.", Citations: []Citation{{Path: "pkg/x.go"}}})
			if err != nil {
				t.Fatal(err)
			}
			if mem.CreatedAt.IsZero() || !mem.UpdatedAt.Equal(mem.VerifiedAt) {
				t.Fatalf("saved memory timestamps = %+v", mem)
			}
			if _, err := store.VerifyMemory(ctx, mem.ID, "abc1234"); err != nil {
				t.Fatal(err)
			}
			if err := store.TouchMemories(ctx, []string{mem.ID, mem.ID}); err != nil {
				t.Fatal(err)
			}
			got, err := open().GetMemory(ctx, mem.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.CommitSHA != "abc1234" || got.UseCount != 1 || got.LastUsedAt == nil || !got.VerifiedAt.After(mem.VerifiedAt) ||
				!reflect.DeepEqual(got.Citations, mem.Citations) || !got.UpdatedAt.Equal(mem.UpdatedAt) {
				t.Fatalf("replayed memory = %+v (saved %+v)", got, mem)
			}
		})
	}
}

func TestSaveMemoryEnforcesCap(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(SQLiteOptions{Path: filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var last *Memory
	for i := 0; i < DefaultMemoryCap; i++ {
		last, err = store.SaveMemory(ctx, SaveMemoryInput{Title: fmt.Sprintf("m%d", i), Body: "b"})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	_, err = store.SaveMemory(ctx, SaveMemoryInput{Title: "over", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "memory_delete") || !strings.Contains(err.Error(), "memory_save") {
		t.Fatalf("over-cap err = %v", err)
	}
	if _, err := store.SaveMemory(ctx, SaveMemoryInput{ID: last.ID, Title: "updated", Body: "b"}); err != nil {
		t.Fatalf("updating at cap should succeed: %v", err)
	}
}

func storeOpeners() map[string]func(t *testing.T) func() Store {
	return map[string]func(t *testing.T) func() Store{
		"filesystem": func(t *testing.T) func() Store {
			dir := filepath.Join(t.TempDir(), "state")
			return func() Store {
				s, err := NewFilesystemStore(FilesystemOptions{StateDir: dir, ProjectID: "p"})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
		},
		"sqlite": func(t *testing.T) func() Store {
			path := filepath.Join(t.TempDir(), "state.db")
			return func() Store {
				s, err := NewSQLiteStore(SQLiteOptions{Path: path, ProjectID: "p"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				return s
			}
		},
	}
}
