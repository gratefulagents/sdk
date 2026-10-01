package projectstate

import (
	"context"
	"encoding/json"
	"time"
)

const SchemaVersion = 2

const (
	TaskStatusOpen       = "open"
	TaskStatusInProgress = "in_progress"
	TaskStatusBlocked    = "blocked"
	TaskStatusClosed     = "closed"
	TaskStatusDeferred   = "deferred"
)

const (
	TaskTypeTask    = "task"
	TaskTypeBug     = "bug"
	TaskTypeFeature = "feature"
	TaskTypeChore   = "chore"
	TaskTypeEpic    = "epic"
)

// Memory kinds (v2). Preferences and decisions are listed first in the
// briefing index; facts and procedures compete for the remaining budget.
const (
	MemoryKindPreference = "preference" // how the user/owner wants work done
	MemoryKindDecision   = "decision"   // durable product/architecture decisions
	MemoryKindFact       = "fact"       // non-obvious facts and gotchas about the project
	MemoryKindProcedure  = "procedure"  // repeatable how-tos
)

// Memory limits. Short, typed entries keep the always-loaded index small and
// force consolidation instead of changelog-style accumulation.
const (
	MaxMemoryTitleLen = 120
	MaxMemoryBodyLen  = 1500
	// DefaultMemoryCap is the soft per-project cap; saves beyond it fail with
	// a request to consolidate or delete.
	DefaultMemoryCap = 200
	// BriefingMemoryBudget bounds the bytes of memory index lines rendered by
	// PrimeContext.
	BriefingMemoryBudget = 2400
	// MemoryStaleAfter is the age of verified_at after which a memory is
	// flagged for re-verification even if its citations are unchanged.
	MemoryStaleAfter = 90 * 24 * time.Hour
)

type Event struct {
	Seq       int64           `json:"seq"`
	EventID   string          `json:"event_id"`
	ProjectID string          `json:"project_id"`
	RunID     string          `json:"run_id,omitempty"`
	Actor     string          `json:"actor,omitempty"`
	Time      time.Time       `json:"time"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type Project struct {
	SchemaVersion int       `json:"schema_version"`
	ProjectID     string    `json:"project_id"`
	WorkDir       string    `json:"workdir,omitempty"`
	StateDir      string    `json:"state_dir,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Task struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Type        string          `json:"type"`
	Status      string          `json:"status"`
	Priority    int             `json:"priority"`
	Assignee    string          `json:"assignee,omitempty"`
	DependsOn   []string        `json:"depends_on,omitempty"`
	Blocks      []string        `json:"blocks,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	Comments    []TaskComment   `json:"comments,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ClosedAt    *time.Time      `json:"closed_at,omitempty"`
	SourceRun   string          `json:"source_run,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

type TaskComment struct {
	ID        string    `json:"id"`
	Actor     string    `json:"actor,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Citation anchors a memory to evidence that can be re-checked later.
type Citation struct {
	// Path is a workspace-relative file path whose content supports the memory.
	Path string `json:"path,omitempty"`
	// URL is an external reference (issue, PR, doc) supporting the memory.
	URL string `json:"url,omitempty"`
}

// Memory is one durable, typed, citable project memory.
type Memory struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Citations []Citation `json:"citations,omitempty"`
	// CommitSHA is the repository HEAD when the memory was last saved or
	// verified; staleness checks diff cited paths against it.
	CommitSHA  string     `json:"commit_sha,omitempty"`
	SourceRun  string     `json:"source_run,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	VerifiedAt time.Time  `json:"verified_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	UseCount   int        `json:"use_count,omitempty"`
}

// MemoryHit is a ranked search result.
type MemoryHit struct {
	Memory
	Score float64 `json:"score"`
}

type CreateTaskInput struct {
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Type        string          `json:"type,omitempty"`
	Priority    int             `json:"priority,omitempty"`
	Assignee    string          `json:"assignee,omitempty"`
	DependsOn   []string        `json:"depends_on,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	SourceRun   string          `json:"source_run,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

type TaskPatch struct {
	Title         *string          `json:"title,omitempty"`
	Description   *string          `json:"description,omitempty"`
	Type          *string          `json:"type,omitempty"`
	Status        *string          `json:"status,omitempty"`
	Priority      *int             `json:"priority,omitempty"`
	Assignee      *string          `json:"assignee,omitempty"`
	Labels        []string         `json:"labels,omitempty"`
	ReplaceLabels bool             `json:"replace_labels,omitempty"`
	Metadata      *json.RawMessage `json:"metadata,omitempty"`
}

type TaskFilter struct {
	Actor           string   `json:"actor,omitempty"`
	Assignee        string   `json:"assignee,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	Limit           int      `json:"limit,omitempty"`
	IncludeAssigned bool     `json:"include_assigned,omitempty"`
}

// SaveMemoryInput creates (empty ID) or fully replaces (existing ID) a memory.
// Stores set UpdatedAt and VerifiedAt to now and preserve CreatedAt, UseCount,
// LastUsedAt and SourceRun (the creator) of an existing memory. Saving an
// unknown non-empty ID fails.
type SaveMemoryInput struct {
	ID        string     `json:"id,omitempty"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Citations []Citation `json:"citations,omitempty"`
	CommitSHA string     `json:"commit_sha,omitempty"`
	SourceRun string     `json:"source_run,omitempty"`
}

// MemoryQuery is a ranked recall request. Query is required.
type MemoryQuery struct {
	Query string   `json:"query"`
	Kinds []string `json:"kinds,omitempty"`
	// Limit defaults to 8 when <= 0.
	Limit int `json:"limit,omitempty"`
}

// MemoryFilter lists memories without ranking.
type MemoryFilter struct {
	Kinds []string `json:"kinds,omitempty"`
	Limit int      `json:"limit,omitempty"`
}

type PrimeOptions struct {
	Actor        string `json:"actor,omitempty"`
	ActiveTaskID string `json:"active_task_id,omitempty"`
	ReadyLimit   int    `json:"ready_limit,omitempty"`
	MemoryLimit  int    `json:"memory_limit,omitempty"`
}

type Store interface {
	TaskStore
	MemoryStore
	PrimeStore
	Close() error
}

type TaskStore interface {
	CreateTask(ctx context.Context, in CreateTaskInput) (*Task, error)
	UpdateTask(ctx context.Context, id string, patch TaskPatch) (*Task, error)
	ClaimTask(ctx context.Context, id, actor string) (*Task, error)
	CloseTask(ctx context.Context, id, reason string) (*Task, error)
	ReadyTasks(ctx context.Context, filter TaskFilter) ([]Task, error)
	ListTasks(ctx context.Context) ([]Task, error)
	GetTask(ctx context.Context, id string) (*Task, error)
	AddDependency(ctx context.Context, taskID, dependsOnID string) error
	RemoveDependency(ctx context.Context, taskID, dependsOnID string) error
	AddComment(ctx context.Context, taskID, actor, body string) (*TaskComment, error)
	// ReleaseClaims reopens every in_progress task assigned to actor, clears
	// the assignee, and records note as a comment. Hosts call it when a run
	// ends so claims never outlive their claimant.
	ReleaseClaims(ctx context.Context, actor, note string) ([]Task, error)
}

type MemoryStore interface {
	// SaveMemory validates (ValidateMemoryInput) and persists a memory.
	SaveMemory(ctx context.Context, in SaveMemoryInput) (*Memory, error)
	GetMemory(ctx context.Context, id string) (*Memory, error)
	// SearchMemories returns hits with Score > 0, best first.
	SearchMemories(ctx context.Context, q MemoryQuery) ([]MemoryHit, error)
	// ListMemories returns memories ordered by kind priority then UpdatedAt desc.
	ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	DeleteMemory(ctx context.Context, id string) error
	// VerifyMemory marks a memory as re-checked: VerifiedAt=now and, when
	// non-empty, CommitSHA=commitSHA.
	VerifyMemory(ctx context.Context, id, commitSHA string) (*Memory, error)
	// TouchMemories records a use: UseCount++ and LastUsedAt=now. Unknown ids
	// are ignored.
	TouchMemories(ctx context.Context, ids []string) error
}

type PrimeStore interface {
	// PrimeContext renders the briefing (see RenderBriefing). Output must be
	// deterministic for a given state so it is prompt-cache friendly.
	PrimeContext(ctx context.Context, opts PrimeOptions) (string, error)
}
