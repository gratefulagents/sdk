package projectstate

import (
	"context"
	"encoding/json"
	"time"
)

const SchemaVersion = 1

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

const (
	MemoryKindPinned     = "pinned"
	MemoryKindSemantic   = "semantic"
	MemoryKindEpisodic   = "episodic"
	MemoryKindProcedural = "procedural"
)

const (
	MemoryScopeProject = "project"
	MemoryScopeUser    = "user"
	MemoryScopeTask    = "task"
	MemoryScopeFile    = "file"
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

type Memory struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Scope      string          `json:"scope"`
	Content    string          `json:"content"`
	Tags       []string        `json:"tags,omitempty"`
	TaskIDs    []string        `json:"task_ids,omitempty"`
	FilePaths  []string        `json:"file_paths,omitempty"`
	SourceRun  string          `json:"source_run,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	LastReadAt *time.Time      `json:"last_read_at,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

type SessionSummary struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id,omitempty"`
	Summary   string    `json:"summary"`
	TaskIDs   []string  `json:"task_ids,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

type UpsertMemoryInput struct {
	ID        string          `json:"id,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Scope     string          `json:"scope,omitempty"`
	Content   string          `json:"content"`
	Tags      []string        `json:"tags,omitempty"`
	TaskIDs   []string        `json:"task_ids,omitempty"`
	FilePaths []string        `json:"file_paths,omitempty"`
	SourceRun string          `json:"source_run,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type MemoryFilter struct {
	Query string   `json:"query,omitempty"`
	Kinds []string `json:"kinds,omitempty"`
	Tags  []string `json:"tags,omitempty"`
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
	SessionStore
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
}

type MemoryStore interface {
	UpsertMemory(ctx context.Context, in UpsertMemoryInput) (*Memory, error)
	SearchMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	DeleteMemory(ctx context.Context, id string) error
}

type SessionStore interface {
	SaveSessionSummary(ctx context.Context, summary SessionSummary) (*SessionSummary, error)
	ListSessionSummaries(ctx context.Context, limit int) ([]SessionSummary, error)
}

type PrimeStore interface {
	PrimeContext(ctx context.Context, opts PrimeOptions) (string, error)
}
