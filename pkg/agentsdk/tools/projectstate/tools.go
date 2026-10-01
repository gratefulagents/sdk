package projectstatetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/projectstate"
)

// Option configures Tools.
type Option func(*baseTool)

// WithWorkDir sets the workspace directory used to record the HEAD commit on
// memory saves and to check cited files for staleness.
func WithWorkDir(dir string) Option {
	return func(t *baseTool) { t.workDir = strings.TrimSpace(dir) }
}

// Tools returns the task, memory, and prime_context tools over store.
func Tools(store projectstate.Store, actor string, opts ...Option) []agentsdk.Tool {
	if store == nil {
		return nil
	}
	base := baseTool{store: store, actor: strings.TrimSpace(actor)}
	for _, opt := range opts {
		if opt != nil {
			opt(&base)
		}
	}
	base.checker = NewGitStalenessChecker(base.workDir)
	return []agentsdk.Tool{
		&taskCreateTool{baseTool: base},
		&taskReadyTool{baseTool: base},
		&taskShowTool{baseTool: base},
		&taskUpdateTool{baseTool: base},
		&taskClaimTool{baseTool: base},
		&taskCloseTool{baseTool: base},
		&taskCommentTool{baseTool: base},
		&taskLinkTool{baseTool: base},
		&memorySearchTool{baseTool: base},
		&memoryGetTool{baseTool: base},
		&memorySaveTool{baseTool: base},
		&memoryVerifyTool{baseTool: base},
		&memoryDeleteTool{baseTool: base},
		&primeContextTool{baseTool: base},
	}
}

type baseTool struct {
	store   projectstate.Store
	actor   string
	workDir string
	checker *GitStalenessChecker
}

func (t baseTool) IsEnabled(*agentsdk.RunContext) bool { return t.store != nil }
func (t baseTool) NeedsApproval() bool                 { return false }
func (t baseTool) TimeoutSeconds() int                 { return 0 }

type taskCreateTool struct{ baseTool }

func (t *taskCreateTool) Name() string { return "task_create" }
func (t *taskCreateTool) Description() string {
	return "Create a durable project task. Use for work that should survive sessions and context compaction."
}
func (t *taskCreateTool) IsReadOnly() bool { return false }
func (t *taskCreateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {"type": "string"},
			"description": {"type": "string"},
			"type": {"type": "string", "enum": ["task", "bug", "feature", "chore", "epic"]},
			"priority": {"type": "integer", "minimum": 0, "maximum": 4},
			"assignee": {"type": "string"},
			"depends_on": {"type": "array", "items": {"type": "string"}},
			"labels": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["title"]
	}`)
}
func (t *taskCreateTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Type        string   `json:"type"`
		Priority    *int     `json:"priority"`
		Assignee    string   `json:"assignee"`
		DependsOn   []string `json:"depends_on"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	priority := 2
	if in.Priority != nil {
		priority = *in.Priority
	}
	task, err := t.store.CreateTask(ctx, projectstate.CreateTaskInput{
		Title:       in.Title,
		Description: in.Description,
		Type:        in.Type,
		Priority:    priority,
		Assignee:    in.Assignee,
		DependsOn:   in.DependsOn,
		Labels:      in.Labels,
	})
	return jsonToolResult(task, err), nil
}

type taskReadyTool struct{ baseTool }

func (t *taskReadyTool) Name() string { return "task_ready" }
func (t *taskReadyTool) Description() string {
	return "List ready durable project tasks: open tasks with no open blockers."
}
func (t *taskReadyTool) IsReadOnly() bool { return true }
func (t *taskReadyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"limit": {"type": "integer"},
			"assignee": {"type": "string"},
			"labels": {"type": "array", "items": {"type": "string"}},
			"include_assigned": {"type": "boolean"}
		}
	}`)
}
func (t *taskReadyTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Limit           int      `json:"limit"`
		Assignee        string   `json:"assignee"`
		Labels          []string `json:"labels"`
		IncludeAssigned bool     `json:"include_assigned"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	tasks, err := t.store.ReadyTasks(ctx, projectstate.TaskFilter{Actor: t.actor, Assignee: in.Assignee, Labels: in.Labels, Limit: in.Limit, IncludeAssigned: in.IncludeAssigned})
	return jsonToolResult(tasks, err), nil
}

type taskShowTool struct{ baseTool }

func (t *taskShowTool) Name() string { return "task_show" }
func (t *taskShowTool) Description() string {
	return "Show one durable project task with comments and dependencies."
}
func (t *taskShowTool) IsReadOnly() bool { return true }
func (t *taskShowTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
}
func (t *taskShowTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.GetTask(ctx, in.ID)
	return jsonToolResult(task, err), nil
}

type taskUpdateTool struct{ baseTool }

func (t *taskUpdateTool) Name() string { return "task_update" }
func (t *taskUpdateTool) Description() string {
	return "Update durable project task fields."
}
func (t *taskUpdateTool) IsReadOnly() bool { return false }
func (t *taskUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"id":{"type":"string"},
			"title":{"type":"string"},
			"description":{"type":"string"},
			"type":{"type":"string"},
			"status":{"type":"string"},
			"priority":{"type":"integer","minimum":0,"maximum":4},
			"assignee":{"type":"string"},
			"labels":{"type":"array","items":{"type":"string"}}
		},
		"required":["id"]
	}`)
}
func (t *taskUpdateTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID          string   `json:"id"`
		Title       *string  `json:"title"`
		Description *string  `json:"description"`
		Type        *string  `json:"type"`
		Status      *string  `json:"status"`
		Priority    *int     `json:"priority"`
		Assignee    *string  `json:"assignee"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.UpdateTask(ctx, in.ID, projectstate.TaskPatch{
		Title:         in.Title,
		Description:   in.Description,
		Type:          in.Type,
		Status:        in.Status,
		Priority:      in.Priority,
		Assignee:      in.Assignee,
		Labels:        in.Labels,
		ReplaceLabels: in.Labels != nil,
	})
	return jsonToolResult(task, err), nil
}

type taskClaimTool struct{ baseTool }

func (t *taskClaimTool) Name() string { return "task_claim" }
func (t *taskClaimTool) Description() string {
	return "Atomically claim a durable project task for the current agent."
}
func (t *taskClaimTool) IsReadOnly() bool { return false }
func (t *taskClaimTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"actor":{"type":"string"}},"required":["id"]}`)
}
func (t *taskClaimTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID    string `json:"id"`
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.ClaimTask(ctx, in.ID, firstNonEmpty(in.Actor, t.actor))
	return jsonToolResult(task, err), nil
}

type taskCloseTool struct{ baseTool }

func (t *taskCloseTool) Name() string { return "task_close" }
func (t *taskCloseTool) Description() string {
	return "Close a durable project task with an optional reason."
}
func (t *taskCloseTool) IsReadOnly() bool { return false }
func (t *taskCloseTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"reason":{"type":"string"}},"required":["id"]}`)
}
func (t *taskCloseTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.CloseTask(ctx, in.ID, in.Reason)
	return jsonToolResult(task, err), nil
}

type taskCommentTool struct{ baseTool }

func (t *taskCommentTool) Name() string { return "task_comment" }
func (t *taskCommentTool) Description() string {
	return "Add a durable comment to a project task."
}
func (t *taskCommentTool) IsReadOnly() bool { return false }
func (t *taskCommentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"body":{"type":"string"},"actor":{"type":"string"}},"required":["id","body"]}`)
}
func (t *taskCommentTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID    string `json:"id"`
		Body  string `json:"body"`
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	comment, err := t.store.AddComment(ctx, in.ID, firstNonEmpty(in.Actor, t.actor), in.Body)
	return jsonToolResult(comment, err), nil
}

type taskLinkTool struct{ baseTool }

func (t *taskLinkTool) Name() string { return "task_link" }
func (t *taskLinkTool) Description() string {
	return "Add or remove a dependency between durable project tasks."
}
func (t *taskLinkTool) IsReadOnly() bool { return false }
func (t *taskLinkTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"depends_on":{"type":"string"},"action":{"type":"string","enum":["add","remove"]}},"required":["id","depends_on"]}`)
}
func (t *taskLinkTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID        string `json:"id"`
		DependsOn string `json:"depends_on"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	var err error
	if strings.EqualFold(in.Action, "remove") {
		err = t.store.RemoveDependency(ctx, in.ID, in.DependsOn)
	} else {
		err = t.store.AddDependency(ctx, in.ID, in.DependsOn)
	}
	return jsonToolResult(map[string]string{"id": in.ID, "depends_on": in.DependsOn, "action": firstNonEmpty(in.Action, "add")}, err), nil
}

const snippetRunes = 240

// StalenessChecker decides whether a memory needs re-verification.
type StalenessChecker interface {
	Check(ctx context.Context, m projectstate.Memory) (stale bool, reason string)
}

const gitTimeout = 5 * time.Second

var commitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// GitStalenessChecker flags memories whose cited files are gone or changed
// since CommitSHA, or whose VerifiedAt is older than MemoryStaleAfter. Without
// a directory only the age rule applies. Git failures never mark a memory
// stale.
type GitStalenessChecker struct {
	dir string
	now func() time.Time
}

func NewGitStalenessChecker(dir string) *GitStalenessChecker {
	return &GitStalenessChecker{dir: strings.TrimSpace(dir), now: time.Now}
}

func (c *GitStalenessChecker) Check(ctx context.Context, m projectstate.Memory) (bool, string) {
	if c.dir != "" {
		var paths []string
		for _, citation := range m.Citations {
			if citation.Path == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(c.dir, filepath.FromSlash(citation.Path))); errors.Is(err, os.ErrNotExist) {
				return true, "cited file " + citation.Path + " no longer exists"
			}
			paths = append(paths, citation.Path)
		}
		if m.CommitSHA != "" && len(paths) > 0 {
			if stale, reason := c.checkCommit(ctx, m.CommitSHA, paths); stale {
				return true, reason
			}
		}
	}
	if !m.VerifiedAt.IsZero() {
		if age := c.now().Sub(m.VerifiedAt); age > projectstate.MemoryStaleAfter {
			return true, fmt.Sprintf("not verified for %d days", int(age.Hours()/24))
		}
	}
	return false, ""
}

func (c *GitStalenessChecker) checkCommit(ctx context.Context, sha string, paths []string) (bool, string) {
	if !commitSHAPattern.MatchString(sha) {
		return true, "verification commit not found"
	}
	if _, err := c.git(ctx, "rev-parse", "--verify", "--quiet", sha+"^{commit}"); err != nil {
		var exitErr *exec.ExitError
		// Exit status 1 is "no such object"; anything else (not a repo, git
		// missing, timeout) is an environment problem, not staleness.
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return true, "verification commit not found"
		}
		return false, ""
	}
	// Compare committed history only (sha..HEAD): memories are stamped with
	// HEAD on save/verify, so uncommitted edits in the working tree must not
	// make a just-saved memory stale.
	out, err := c.git(ctx, append([]string{"--literal-pathspecs", "diff", "--name-only", "--relative", sha, "HEAD", "--"}, paths...)...)
	if err != nil {
		return false, ""
	}
	changed := strings.Fields(out)
	if len(changed) == 0 {
		return false, ""
	}
	return true, "cited files changed since verification: " + strings.Join(changed, ", ")
}

// HeadSHA returns the repository HEAD commit, or "" when unavailable.
func (c *GitStalenessChecker) HeadSHA(ctx context.Context) string {
	if c.dir == "" {
		return ""
	}
	out, err := c.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (c *GitStalenessChecker) git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", c.dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

type memorySearchTool struct{ baseTool }

func (t *memorySearchTool) Name() string { return "memory_search" }
func (t *memorySearchTool) Description() string {
	return "Search durable project memory (user preferences, decisions, facts/gotchas, procedures saved by earlier runs). " +
		"Search before re-investigating anything the project may already know, and before saving a new memory. " +
		"Returns ranked hits with a body snippet; use memory_get for the full text. Hits marked stale must be re-verified before you rely on them."
}
func (t *memorySearchTool) IsReadOnly() bool { return true }
func (t *memorySearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Keywords or a natural-language question."},
			"kinds": {"type": "array", "items": {"type": "string", "enum": ["preference", "decision", "fact", "procedure"]}, "description": "Optional kind filter."},
			"limit": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum hits (default 8)."}
		},
		"required": ["query"]
	}`)
}

type memorySearchHit struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	Snippet     string  `json:"snippet"`
	Score       float64 `json:"score"`
	Stale       bool    `json:"stale,omitempty"`
	StaleReason string  `json:"stale_reason,omitempty"`
}

func (t *memorySearchTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Query string   `json:"query"`
		Kinds []string `json:"kinds"`
		Limit int      `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	hits, err := t.store.SearchMemories(ctx, projectstate.MemoryQuery{Query: in.Query, Kinds: in.Kinds, Limit: in.Limit})
	if err != nil {
		return jsonToolResult(nil, err), nil
	}
	out := make([]memorySearchHit, 0, len(hits))
	for _, hit := range hits {
		stale, reason := t.checker.Check(ctx, hit.Memory)
		out = append(out, memorySearchHit{
			ID:          hit.ID,
			Kind:        hit.Kind,
			Title:       hit.Title,
			Snippet:     snippet(hit.Body, snippetRunes),
			Score:       math.Round(hit.Score*100) / 100,
			Stale:       stale,
			StaleReason: reason,
		})
	}
	return jsonToolResult(out, nil), nil
}

type memoryGetTool struct{ baseTool }

func (t *memoryGetTool) Name() string { return "memory_get" }
func (t *memoryGetTool) Description() string {
	return "Read one durable project memory in full by id (ids come from the briefing's Memory Index or memory_search). " +
		"If the result is stale, re-verify it against its cited files/URLs before relying on it: " +
		"call memory_verify if it still holds, memory_save with its id to correct it, or memory_delete if it is obsolete."
}
func (t *memoryGetTool) IsReadOnly() bool { return true }
func (t *memoryGetTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Memory id, e.g. mem_ab12cd34ef56."}},"required":["id"]}`)
}

type memoryWithStaleness struct {
	projectstate.Memory
	Stale       bool   `json:"stale"`
	StaleReason string `json:"stale_reason,omitempty"`
}

func (t *memoryGetTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	mem, err := t.store.GetMemory(ctx, in.ID)
	if err != nil {
		return jsonToolResult(nil, err), nil
	}
	// Usage tracking is best-effort; a failed touch must not hide the memory.
	_ = t.store.TouchMemories(ctx, []string{mem.ID})
	return jsonToolResult(t.withStaleness(ctx, *mem), nil), nil
}

type memorySaveTool struct{ baseTool }

func (t *memorySaveTool) Name() string { return "memory_save" }
func (t *memorySaveTool) Description() string {
	return "Save durable project knowledge for future runs. Save only what is reusable later and not obvious from the code or docs: " +
		"user preferences, decisions with their rationale, non-obvious facts and gotchas, and repeatable procedures. " +
		"Never save progress logs, task status, or PR/commit changelogs. " +
		"Keep the title to one line and the body short; cite the files (workspace-relative paths) or URLs that support it so it can be re-verified later. " +
		"Prefer updating an existing memory over adding a new one: search first, then pass its id to replace it (omitted fields keep their current values). " +
		"Without an id a new memory is created; near-duplicates of existing memories are rejected unless allow_duplicate is true."
}
func (t *memorySaveTool) IsReadOnly() bool { return false }
func (t *memorySaveTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Existing memory id to replace. Omit to create a new memory."},
			"kind": {"type": "string", "enum": ["preference", "decision", "fact", "procedure"], "description": "preference: how the user wants work done; decision: a durable choice and its rationale; fact: a non-obvious fact or gotcha; procedure: a repeatable how-to."},
			"title": {"type": "string", "description": "One-line summary (max 120 characters)."},
			"body": {"type": "string", "description": "The reusable knowledge (max 1500 characters). For decisions include the rationale."},
			"citations": {
				"type": "array",
				"description": "Evidence supporting the memory.",
				"items": {
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "Workspace-relative file path."},
						"url": {"type": "string", "description": "Issue, PR, or documentation URL."}
					}
				}
			},
			"allow_duplicate": {"type": "boolean", "description": "Create even when a similar memory exists. Only for genuinely distinct knowledge."}
		}
	}`)
}

func (t *memorySaveTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID             string                   `json:"id"`
		Kind           *string                  `json:"kind"`
		Title          *string                  `json:"title"`
		Body           *string                  `json:"body"`
		Citations      *[]projectstate.Citation `json:"citations"`
		AllowDuplicate bool                     `json:"allow_duplicate"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	save := projectstate.SaveMemoryInput{ID: strings.TrimSpace(in.ID), SourceRun: t.actor}
	if save.ID != "" {
		existing, err := t.store.GetMemory(ctx, save.ID)
		if err != nil {
			return jsonToolResult(nil, err), nil
		}
		save.Kind, save.Title, save.Body = existing.Kind, existing.Title, existing.Body
		save.Citations = existing.Citations
		save.CommitSHA = existing.CommitSHA
	}
	if in.Kind != nil {
		save.Kind = *in.Kind
	}
	if in.Title != nil {
		save.Title = *in.Title
	}
	if in.Body != nil {
		save.Body = *in.Body
	}
	if in.Citations != nil {
		save.Citations = *in.Citations
	}
	if t.workDir != "" {
		save.CommitSHA = t.checker.HeadSHA(ctx)
	}
	if err := projectstate.ValidateMemoryInput(&save); err != nil {
		return jsonToolResult(nil, err), nil
	}
	if save.ID == "" && !in.AllowDuplicate {
		if dupes := t.duplicates(ctx, save.Title, save.Body); len(dupes) > 0 {
			var b strings.Builder
			b.WriteString("Not saved: this looks like a duplicate of existing memory:\n")
			for _, hit := range dupes {
				fmt.Fprintf(&b, "- %s [%s] %s\n", hit.ID, hit.Kind, hit.Title)
			}
			b.WriteString("Update one of them instead by calling memory_save with its id (read it first with memory_get). " +
				"Set allow_duplicate=true only if this is genuinely distinct knowledge.")
			return agentsdk.ToolResult{Content: b.String(), IsError: true}, nil
		}
	}
	mem, err := t.store.SaveMemory(ctx, save)
	if err != nil {
		return jsonToolResult(nil, err), nil
	}
	return jsonToolResult(struct {
		Memory  *projectstate.Memory `json:"memory"`
		Warning string               `json:"warning,omitempty"`
	}{mem, projectstate.ProgressLogWarning(mem.Title, mem.Body)}, nil), nil
}

func (t *memorySaveTool) duplicates(ctx context.Context, title, body string) []projectstate.MemoryHit {
	hits, err := t.store.SearchMemories(ctx, projectstate.MemoryQuery{Query: title + " " + body, Limit: 5})
	if err != nil {
		return nil
	}
	var out []projectstate.MemoryHit
	for _, hit := range hits {
		if projectstate.Similarity(title, body, hit.Title, hit.Body) >= projectstate.DuplicateThreshold {
			out = append(out, hit)
		}
	}
	return out
}

type memoryVerifyTool struct{ baseTool }

func (t *memoryVerifyTool) Name() string { return "memory_verify" }
func (t *memoryVerifyTool) Description() string {
	return "Mark a memory as re-verified. Call it only after re-checking a stale memory against its cited files/URLs and confirming it is still true; " +
		"it records the current commit and clears the age-based staleness. If the memory is wrong, use memory_save with its id or memory_delete instead."
}
func (t *memoryVerifyTool) IsReadOnly() bool { return false }
func (t *memoryVerifyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
}
func (t *memoryVerifyTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	mem, err := t.store.VerifyMemory(ctx, in.ID, t.checker.HeadSHA(ctx))
	if err != nil {
		return jsonToolResult(nil, err), nil
	}
	return jsonToolResult(t.withStaleness(ctx, *mem), nil), nil
}

type memoryDeleteTool struct{ baseTool }

func (t *memoryDeleteTool) Name() string { return "memory_delete" }
func (t *memoryDeleteTool) Description() string {
	return "Delete a durable project memory that is obsolete, wrong, or superseded by another memory."
}
func (t *memoryDeleteTool) IsReadOnly() bool { return false }
func (t *memoryDeleteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"reason":{"type":"string","description":"Why the memory is no longer valid."}},"required":["id"]}`)
}
func (t *memoryDeleteTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	err := t.store.DeleteMemory(ctx, in.ID)
	out := map[string]string{"id": strings.TrimSpace(in.ID), "status": "deleted"}
	if reason := strings.TrimSpace(in.Reason); reason != "" {
		out["reason"] = reason
	}
	return jsonToolResult(out, err), nil
}

func (t baseTool) withStaleness(ctx context.Context, mem projectstate.Memory) memoryWithStaleness {
	stale, reason := t.checker.Check(ctx, mem)
	return memoryWithStaleness{Memory: mem, Stale: stale, StaleReason: reason}
}

func snippet(body string, max int) string {
	body = strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(body) <= max {
		return body
	}
	return string([]rune(body)[:max]) + "..."
}

type primeContextTool struct{ baseTool }

func (t *primeContextTool) Name() string { return "prime_context" }
func (t *primeContextTool) Description() string {
	return "Build compact durable project context from tasks and memories for session start or compaction recovery."
}
func (t *primeContextTool) IsReadOnly() bool { return true }
func (t *primeContextTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"active_task_id":{"type":"string"},"ready_limit":{"type":"integer"},"memory_limit":{"type":"integer"}}}`)
}
func (t *primeContextTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in projectstate.PrimeOptions
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	in.Actor = firstNonEmpty(in.Actor, t.actor)
	text, err := t.store.PrimeContext(ctx, in)
	if err != nil {
		return errorResult("%v", err), nil
	}
	return agentsdk.ToolResult{Content: text}, nil
}

func jsonToolResult(value any, err error) agentsdk.ToolResult {
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}
	}
	return agentsdk.ToolResult{Content: string(data)}
}

func errorResult(format string, args ...any) agentsdk.ToolResult {
	return agentsdk.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
