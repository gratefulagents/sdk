package projectstate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const (
	eventsFileName = "events.jsonl"
	lockStaleAfter = 30 * time.Minute
)

type FilesystemOptions struct {
	StateDir  string
	ProjectID string
	WorkDir   string
	Actor     string
	RunID     string
	// Embedder enables embeddings-backed hybrid memory recall. When nil,
	// SearchMemories falls back to the lexical keyword search and behaves
	// exactly as before.
	Embedder Embedder
	// Hybrid tunes lexical/semantic fusion. When nil, DefaultHybridConfig is
	// used. Ignored when Embedder is nil.
	Hybrid *HybridConfig
}

type FilesystemStore struct {
	mu        sync.Mutex
	stateDir  string
	projectID string
	workDir   string
	actor     string
	runID     string
	embedder  Embedder
	hybrid    HybridConfig
}

type state struct {
	project  Project
	tasks    map[string]Task
	memories map[string]Memory
	sessions map[string]SessionSummary
	lastSeq  int64
}

type tasksIndex struct {
	SchemaVersion int       `json:"schema_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	Tasks         []Task    `json:"tasks"`
}

type memoriesIndex struct {
	SchemaVersion int       `json:"schema_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	Memories      []Memory  `json:"memories"`
}

type sessionsIndex struct {
	SchemaVersion int              `json:"schema_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Sessions      []SessionSummary `json:"sessions"`
}

func NewFilesystemStore(opts FilesystemOptions) (*FilesystemStore, error) {
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir != "" {
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = abs
		}
	}
	projectID := sanitizeProjectID(opts.ProjectID)
	if projectID == "" {
		projectID = DeriveProjectID(workDir)
	}
	stateDir := strings.TrimSpace(opts.StateDir)
	var err error
	if stateDir == "" {
		stateDir, err = DefaultStateDir(projectID)
		if err != nil {
			return nil, err
		}
	} else if stateDir, err = filepath.Abs(stateDir); err != nil {
		return nil, fmt.Errorf("resolve project state dir: %w", err)
	}
	s := &FilesystemStore{
		stateDir:  stateDir,
		projectID: projectID,
		workDir:   workDir,
		actor:     strings.TrimSpace(opts.Actor),
		runID:     strings.TrimSpace(opts.RunID),
		embedder:  opts.Embedder,
		hybrid:    DefaultHybridConfig(),
	}
	if opts.Hybrid != nil {
		s.hybrid = *opts.Hybrid
	}
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	if err := s.withLockedState(context.Background(), func(st *state) error {
		now := time.Now().UTC()
		if st.project.ProjectID == "" {
			st.project = Project{
				SchemaVersion: SchemaVersion,
				ProjectID:     projectID,
				WorkDir:       workDir,
				StateDir:      stateDir,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
		}
		if st.lastSeq == 0 {
			st.project.UpdatedAt = now
			if err := s.appendEventLocked(st, "project.initialized", st.project, now); err != nil {
				return err
			}
		}
		return s.writeIndexesLocked(st)
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FilesystemStore) Close() error { return nil }

func DefaultStateDir(projectID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".gratefulagents", "projects", sanitizeProjectID(projectID), "state"), nil
}

func DeriveProjectID(workDir string) string {
	trimmed := strings.TrimSpace(workDir)
	if trimmed == "" {
		trimmed = "project"
	}
	base := filepath.Base(trimmed)
	if base == "." || base == string(filepath.Separator) || strings.TrimSpace(base) == "" {
		base = "project"
	}
	sum := sha1.Sum([]byte(trimmed))
	return sanitizeProjectID(base) + "-" + hex.EncodeToString(sum[:])[:8]
}

func (s *FilesystemStore) StateDir() string  { return s.stateDir }
func (s *FilesystemStore) ProjectID() string { return s.projectID }

func (s *FilesystemStore) CreateTask(ctx context.Context, in CreateTaskInput) (*Task, error) {
	var out *Task
	err := s.mutate(ctx, "task.created", func(st *state, now time.Time) (any, error) {
		title := strings.TrimSpace(in.Title)
		if title == "" {
			return nil, fmt.Errorf("title is required")
		}
		task := Task{
			ID:          newID("task"),
			Title:       title,
			Description: strings.TrimSpace(in.Description),
			Type:        normalizeTaskType(in.Type),
			Status:      TaskStatusOpen,
			Priority:    normalizePriority(in.Priority),
			Assignee:    strings.TrimSpace(in.Assignee),
			DependsOn:   uniqueNonEmpty(in.DependsOn),
			Labels:      uniqueNonEmpty(in.Labels),
			CreatedAt:   now,
			UpdatedAt:   now,
			SourceRun:   firstNonEmpty(in.SourceRun, s.runID),
			Metadata:    cloneRaw(in.Metadata),
		}
		st.tasks[task.ID] = task
		recomputeBlocks(st)
		out = cloneTaskPtr(task)
		return task, nil
	})
	return out, err
}

func (s *FilesystemStore) UpdateTask(ctx context.Context, id string, patch TaskPatch) (*Task, error) {
	var out *Task
	err := s.mutate(ctx, "task.updated", func(st *state, now time.Time) (any, error) {
		task, ok := st.tasks[strings.TrimSpace(id)]
		if !ok {
			return nil, fmt.Errorf("task %q not found", id)
		}
		applyPatch(&task, patch, now)
		st.tasks[task.ID] = task
		recomputeBlocks(st)
		out = cloneTaskPtr(task)
		return taskUpdatePayload{ID: task.ID, Patch: patch, Task: task}, nil
	})
	return out, err
}

func (s *FilesystemStore) ClaimTask(ctx context.Context, id, actor string) (*Task, error) {
	var out *Task
	err := s.mutate(ctx, "task.claimed", func(st *state, now time.Time) (any, error) {
		task, ok := st.tasks[strings.TrimSpace(id)]
		if !ok {
			return nil, fmt.Errorf("task %q not found", id)
		}
		claimant := firstNonEmpty(actor, s.actor)
		if claimant == "" {
			claimant = "agent"
		}
		task.Assignee = claimant
		task.Status = TaskStatusInProgress
		task.UpdatedAt = now
		task.ClosedAt = nil
		st.tasks[task.ID] = task
		recomputeBlocks(st)
		out = cloneTaskPtr(task)
		return taskClaimedPayload{ID: task.ID, Actor: claimant, At: now}, nil
	})
	return out, err
}

func (s *FilesystemStore) CloseTask(ctx context.Context, id, reason string) (*Task, error) {
	var out *Task
	err := s.mutate(ctx, "task.closed", func(st *state, now time.Time) (any, error) {
		task, ok := st.tasks[strings.TrimSpace(id)]
		if !ok {
			return nil, fmt.Errorf("task %q not found", id)
		}
		task.Status = TaskStatusClosed
		task.UpdatedAt = now
		task.ClosedAt = &now
		if strings.TrimSpace(reason) != "" {
			task.Comments = append(task.Comments, TaskComment{
				ID:        newID("comment"),
				Actor:     s.actor,
				Body:      "Closed: " + strings.TrimSpace(reason),
				CreatedAt: now,
			})
		}
		st.tasks[task.ID] = task
		recomputeBlocks(st)
		out = cloneTaskPtr(task)
		return taskClosedPayload{ID: task.ID, Reason: strings.TrimSpace(reason), At: now, Task: task}, nil
	})
	return out, err
}

func (s *FilesystemStore) ReadyTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(st.tasks))
	for _, task := range st.tasks {
		if task.Status != TaskStatusOpen {
			continue
		}
		if !matchesLabels(task.Labels, filter.Labels) {
			continue
		}
		actor := firstNonEmpty(filter.Actor, filter.Assignee)
		if filter.Assignee != "" && task.Assignee != "" && task.Assignee != filter.Assignee {
			continue
		}
		if !filter.IncludeAssigned && task.Assignee != "" && task.Assignee != actor {
			continue
		}
		if hasOpenBlocker(st, task) {
			continue
		}
		tasks = append(tasks, cloneTask(task))
	}
	sortTasks(tasks)
	return limitTasks(tasks, filter.Limit), nil
}

func (s *FilesystemStore) ListTasks(ctx context.Context) ([]Task, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(st.tasks))
	for _, task := range st.tasks {
		tasks = append(tasks, cloneTask(task))
	}
	sortTasks(tasks)
	return tasks, nil
}

func (s *FilesystemStore) GetTask(ctx context.Context, id string) (*Task, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	task, ok := st.tasks[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("task %q not found", id)
	}
	return cloneTaskPtr(task), nil
}

func (s *FilesystemStore) AddDependency(ctx context.Context, taskID, dependsOnID string) error {
	return s.mutate(ctx, "task.dependency_added", func(st *state, now time.Time) (any, error) {
		taskID = strings.TrimSpace(taskID)
		dependsOnID = strings.TrimSpace(dependsOnID)
		task, ok := st.tasks[taskID]
		if !ok {
			return nil, fmt.Errorf("task %q not found", taskID)
		}
		if _, ok := st.tasks[dependsOnID]; !ok {
			return nil, fmt.Errorf("dependency task %q not found", dependsOnID)
		}
		if taskID == dependsOnID {
			return nil, fmt.Errorf("task cannot depend on itself")
		}
		task.DependsOn = appendUnique(task.DependsOn, dependsOnID)
		task.UpdatedAt = now
		st.tasks[task.ID] = task
		recomputeBlocks(st)
		return dependencyPayload{ID: taskID, DependsOn: dependsOnID, At: now}, nil
	})
}

func (s *FilesystemStore) RemoveDependency(ctx context.Context, taskID, dependsOnID string) error {
	return s.mutate(ctx, "task.dependency_removed", func(st *state, now time.Time) (any, error) {
		taskID = strings.TrimSpace(taskID)
		dependsOnID = strings.TrimSpace(dependsOnID)
		task, ok := st.tasks[taskID]
		if !ok {
			return nil, fmt.Errorf("task %q not found", taskID)
		}
		task.DependsOn = removeString(task.DependsOn, dependsOnID)
		task.UpdatedAt = now
		st.tasks[task.ID] = task
		recomputeBlocks(st)
		return dependencyPayload{ID: taskID, DependsOn: dependsOnID, At: now}, nil
	})
}

func (s *FilesystemStore) AddComment(ctx context.Context, taskID, actor, body string) (*TaskComment, error) {
	var out *TaskComment
	err := s.mutate(ctx, "task.comment_added", func(st *state, now time.Time) (any, error) {
		taskID = strings.TrimSpace(taskID)
		task, ok := st.tasks[taskID]
		if !ok {
			return nil, fmt.Errorf("task %q not found", taskID)
		}
		body = strings.TrimSpace(body)
		if body == "" {
			return nil, fmt.Errorf("comment body is required")
		}
		comment := TaskComment{ID: newID("comment"), Actor: firstNonEmpty(actor, s.actor), Body: body, CreatedAt: now}
		task.Comments = append(task.Comments, comment)
		task.UpdatedAt = now
		st.tasks[task.ID] = task
		out = &comment
		return taskCommentPayload{ID: taskID, Comment: comment}, nil
	})
	return out, err
}

func (s *FilesystemStore) UpsertMemory(ctx context.Context, in UpsertMemoryInput) (*Memory, error) {
	var out *Memory
	err := s.mutate(ctx, "memory.upserted", func(st *state, now time.Time) (any, error) {
		content := strings.TrimSpace(in.Content)
		if content == "" {
			return nil, fmt.Errorf("memory content is required")
		}
		id := strings.TrimSpace(in.ID)
		createdAt := now
		if existing, ok := st.memories[id]; ok {
			createdAt = existing.CreatedAt
		}
		if id == "" {
			id = newID("mem")
		}
		mem := Memory{
			ID:        id,
			Kind:      normalizeMemoryKind(in.Kind),
			Scope:     normalizeMemoryScope(in.Scope),
			Content:   content,
			Tags:      uniqueNonEmpty(in.Tags),
			TaskIDs:   uniqueNonEmpty(in.TaskIDs),
			FilePaths: uniqueNonEmpty(in.FilePaths),
			SourceRun: firstNonEmpty(in.SourceRun, s.runID),
			CreatedAt: createdAt,
			UpdatedAt: now,
			Metadata:  cloneRaw(in.Metadata),
		}
		st.memories[mem.ID] = mem
		out = cloneMemoryPtr(mem)
		return mem, nil
	})
	if err != nil {
		return nil, err
	}
	if out != nil {
		// Best-effort: caching the embedding must never fail the write. A
		// missing vector is backfilled lazily on the next recall.
		_ = s.cacheMemoryEmbedding(ctx, out.ID, out.Content)
	}
	return out, err
}

func (s *FilesystemStore) SearchMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	if strings.TrimSpace(filter.Query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	if s.embedder == nil {
		return s.listMemories(ctx, filter, true)
	}
	return s.searchHybrid(ctx, filter)
}

// searchHybrid ranks memories by fusing the lexical keyword signal with cosine
// similarity over cached embeddings. Candidates are filtered by kind and tags
// but not by keyword, so semantically relevant memories surface even when they
// share no exact terms with the query.
func (s *FilesystemStore) searchHybrid(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []Memory
	for _, mem := range st.memories {
		if !matchesAny(mem.Kind, filter.Kinds) || !matchesLabels(mem.Tags, filter.Tags) {
			continue
		}
		candidates = append(candidates, mem)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	vectors := s.ensureEmbeddings(ctx, candidates)
	var queryVec []float32
	if vecs, embErr := s.embedder.Embed(ctx, []string{filter.Query}); embErr == nil && len(vecs) == 1 {
		queryVec = vecs[0]
	}
	ranked := rankHybrid(filter.Query, candidates, queryVec, vectors, s.hybrid, time.Now().UTC())
	if filter.Limit > 0 && len(ranked) > filter.Limit {
		ranked = ranked[:filter.Limit]
	}
	return ranked, nil
}

func (s *FilesystemStore) ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	return s.listMemories(ctx, filter, false)
}

func (s *FilesystemStore) DeleteMemory(ctx context.Context, id string) error {
	return s.mutate(ctx, "memory.deleted", func(st *state, now time.Time) (any, error) {
		id = strings.TrimSpace(id)
		if _, ok := st.memories[id]; !ok {
			return nil, fmt.Errorf("memory %q not found", id)
		}
		delete(st.memories, id)
		return memoryDeletedPayload{ID: id, At: now}, nil
	})
}

func (s *FilesystemStore) SaveSessionSummary(ctx context.Context, summary SessionSummary) (*SessionSummary, error) {
	var out *SessionSummary
	err := s.mutate(ctx, "session.summary_saved", func(st *state, now time.Time) (any, error) {
		if strings.TrimSpace(summary.Summary) == "" {
			return nil, fmt.Errorf("session summary is required")
		}
		if strings.TrimSpace(summary.ID) == "" {
			summary.ID = newID("session")
			summary.CreatedAt = now
		}
		if existing, ok := st.sessions[summary.ID]; ok && !existing.CreatedAt.IsZero() {
			summary.CreatedAt = existing.CreatedAt
		}
		summary.RunID = firstNonEmpty(summary.RunID, s.runID)
		summary.UpdatedAt = now
		summary.TaskIDs = uniqueNonEmpty(summary.TaskIDs)
		st.sessions[summary.ID] = summary
		cp := cloneSession(summary)
		out = &cp
		return summary, nil
	})
	return out, err
}

func (s *FilesystemStore) ListSessionSummaries(ctx context.Context, limit int) ([]SessionSummary, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(st.sessions))
	for _, session := range st.sessions {
		out = append(out, cloneSession(session))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *FilesystemStore) listMemories(ctx context.Context, filter MemoryFilter, requireQuery bool) ([]Memory, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	if requireQuery && query == "" {
		return nil, fmt.Errorf("query is required")
	}
	var out []Memory
	for _, mem := range st.memories {
		if !matchesAny(mem.Kind, filter.Kinds) || !matchesLabels(mem.Tags, filter.Tags) {
			continue
		}
		if query != "" && !memoryMatchesQuery(mem, query) {
			continue
		}
		out = append(out, cloneMemory(mem))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind == MemoryKindPinned && out[j].Kind != MemoryKindPinned {
			return true
		}
		if out[i].Kind != MemoryKindPinned && out[j].Kind == MemoryKindPinned {
			return false
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *FilesystemStore) mutate(ctx context.Context, eventType string, fn func(*state, time.Time) (any, error)) error {
	return s.withLockedState(ctx, func(st *state) error {
		now := time.Now().UTC()
		payload, err := fn(st, now)
		if err != nil {
			return err
		}
		st.project.UpdatedAt = now
		if err := s.appendEventLocked(st, eventType, payload, now); err != nil {
			return err
		}
		return s.writeIndexesLocked(st)
	})
}

func (s *FilesystemStore) loadState(ctx context.Context) (*state, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.acquireLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.loadStateLocked()
}

func (s *FilesystemStore) withLockedState(ctx context.Context, fn func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDirs(); err != nil {
		return err
	}
	release, err := s.acquireLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	st, err := s.loadStateLocked()
	if err != nil {
		return err
	}
	if st.project.ProjectID == "" {
		now := time.Now().UTC()
		st.project = Project{SchemaVersion: SchemaVersion, ProjectID: s.projectID, WorkDir: s.workDir, StateDir: s.stateDir, CreatedAt: now, UpdatedAt: now}
	}
	return fn(st)
}

func (s *FilesystemStore) ensureDirs() error {
	for _, dir := range []string{s.stateDir, filepath.Join(s.stateDir, "indexes"), filepath.Join(s.stateDir, "snapshots"), filepath.Join(s.stateDir, "locks")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func (s *FilesystemStore) acquireLock(ctx context.Context) (func(), error) {
	lockPath := filepath.Join(s.stateDir, "locks", "state.lock")
	for {
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "pid=%d\ntime=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire project state lock: %w", err)
		}
		if staleProjectStateLock(lockPath) {
			_ = os.Remove(lockPath)
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func staleProjectStateLock(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) > lockStaleAfter {
		return true
	}
	pid, ok := projectStateLockPID(path)
	return ok && !projectStateLockProcessAlive(pid)
}

func projectStateLockPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "pid=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(value))
		return pid, err == nil && pid > 0
	}
	return 0, false
}

func projectStateLockProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

func (s *FilesystemStore) loadStateLocked() (*state, error) {
	st := &state{
		tasks:    make(map[string]Task),
		memories: make(map[string]Memory),
		sessions: make(map[string]SessionSummary),
	}
	path := filepath.Join(s.stateDir, eventsFileName)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, fmt.Errorf("open events: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("parse event: %w", err)
		}
		if ev.Seq > st.lastSeq {
			st.lastSeq = ev.Seq
		}
		if err := applyEvent(st, ev); err != nil {
			return nil, fmt.Errorf("apply event %d %s: %w", ev.Seq, ev.Type, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan events: %w", err)
	}
	recomputeBlocks(st)
	return st, nil
}

func (s *FilesystemStore) appendEventLocked(st *state, eventType string, payload any, now time.Time) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	ev := Event{
		Seq:       st.lastSeq + 1,
		EventID:   newID("evt"),
		ProjectID: s.projectID,
		RunID:     s.runID,
		Actor:     s.actor,
		Time:      now,
		Type:      eventType,
		Payload:   raw,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	path := filepath.Join(s.stateDir, eventsFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events for append: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	st.lastSeq = ev.Seq
	return nil
}

func (s *FilesystemStore) writeIndexesLocked(st *state) error {
	now := time.Now().UTC()
	st.project.SchemaVersion = SchemaVersion
	st.project.ProjectID = firstNonEmpty(st.project.ProjectID, s.projectID)
	st.project.WorkDir = firstNonEmpty(st.project.WorkDir, s.workDir)
	st.project.StateDir = s.stateDir
	if st.project.CreatedAt.IsZero() {
		st.project.CreatedAt = now
	}
	if st.project.UpdatedAt.IsZero() {
		st.project.UpdatedAt = now
	}

	tasks := make([]Task, 0, len(st.tasks))
	for _, task := range st.tasks {
		tasks = append(tasks, cloneTask(task))
	}
	sortTasks(tasks)
	memories := make([]Memory, 0, len(st.memories))
	for _, mem := range st.memories {
		memories = append(memories, cloneMemory(mem))
	}
	sort.SliceStable(memories, func(i, j int) bool { return memories[i].UpdatedAt.After(memories[j].UpdatedAt) })
	sessions := make([]SessionSummary, 0, len(st.sessions))
	for _, summary := range st.sessions {
		sessions = append(sessions, cloneSession(summary))
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })

	if err := writeJSONAtomic(filepath.Join(s.stateDir, "indexes", "project.json"), st.project); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(s.stateDir, "indexes", "tasks.json"), tasksIndex{SchemaVersion: SchemaVersion, UpdatedAt: now, Tasks: tasks}); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(s.stateDir, "indexes", "memories.json"), memoriesIndex{SchemaVersion: SchemaVersion, UpdatedAt: now, Memories: memories}); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(s.stateDir, "indexes", "sessions.json"), sessionsIndex{SchemaVersion: SchemaVersion, UpdatedAt: now, Sessions: sessions}); err != nil {
		return err
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("create temp %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func applyEvent(st *state, ev Event) error {
	switch ev.Type {
	case "project.initialized":
		var project Project
		if err := json.Unmarshal(ev.Payload, &project); err != nil {
			return err
		}
		st.project = project
	case "task.created":
		var task Task
		if err := json.Unmarshal(ev.Payload, &task); err != nil {
			return err
		}
		st.tasks[task.ID] = task
	case "task.updated":
		var p taskUpdatePayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		if p.Task.ID != "" {
			st.tasks[p.Task.ID] = p.Task
		}
	case "task.claimed":
		var p taskClaimedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		task := st.tasks[p.ID]
		task.Assignee = p.Actor
		task.Status = TaskStatusInProgress
		task.UpdatedAt = p.At
		task.ClosedAt = nil
		st.tasks[task.ID] = task
	case "task.closed":
		var p taskClosedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		if p.Task.ID != "" {
			st.tasks[p.Task.ID] = p.Task
			return nil
		}
		task := st.tasks[p.ID]
		task.Status = TaskStatusClosed
		task.UpdatedAt = p.At
		task.ClosedAt = &p.At
		if p.Reason != "" {
			task.Comments = append(task.Comments, TaskComment{ID: newID("comment"), Actor: ev.Actor, Body: "Closed: " + p.Reason, CreatedAt: p.At})
		}
		st.tasks[task.ID] = task
	case "task.comment_added":
		var p taskCommentPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		task := st.tasks[p.ID]
		task.Comments = append(task.Comments, p.Comment)
		task.UpdatedAt = p.Comment.CreatedAt
		st.tasks[task.ID] = task
	case "task.dependency_added":
		var p dependencyPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		task := st.tasks[p.ID]
		task.DependsOn = appendUnique(task.DependsOn, p.DependsOn)
		task.UpdatedAt = p.At
		st.tasks[task.ID] = task
	case "task.dependency_removed":
		var p dependencyPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		task := st.tasks[p.ID]
		task.DependsOn = removeString(task.DependsOn, p.DependsOn)
		task.UpdatedAt = p.At
		st.tasks[task.ID] = task
	case "memory.upserted":
		var mem Memory
		if err := json.Unmarshal(ev.Payload, &mem); err != nil {
			return err
		}
		st.memories[mem.ID] = mem
	case "memory.deleted":
		var p memoryDeletedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		delete(st.memories, p.ID)
	case "session.summary_saved":
		var summary SessionSummary
		if err := json.Unmarshal(ev.Payload, &summary); err != nil {
			return err
		}
		st.sessions[summary.ID] = summary
	default:
		return nil
	}
	return nil
}

type taskUpdatePayload struct {
	ID    string    `json:"id"`
	Patch TaskPatch `json:"patch"`
	Task  Task      `json:"task"`
}

type taskClaimedPayload struct {
	ID    string    `json:"id"`
	Actor string    `json:"actor"`
	At    time.Time `json:"at"`
}

type taskClosedPayload struct {
	ID     string    `json:"id"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
	Task   Task      `json:"task,omitempty"`
}

type taskCommentPayload struct {
	ID      string      `json:"id"`
	Comment TaskComment `json:"comment"`
}

type dependencyPayload struct {
	ID        string    `json:"id"`
	DependsOn string    `json:"depends_on"`
	At        time.Time `json:"at"`
}

type memoryDeletedPayload struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

func applyPatch(task *Task, patch TaskPatch, now time.Time) {
	if patch.Title != nil {
		task.Title = strings.TrimSpace(*patch.Title)
	}
	if patch.Description != nil {
		task.Description = strings.TrimSpace(*patch.Description)
	}
	if patch.Type != nil {
		task.Type = normalizeTaskType(*patch.Type)
	}
	if patch.Status != nil {
		task.Status = normalizeTaskStatus(*patch.Status)
		if task.Status == TaskStatusClosed {
			task.ClosedAt = &now
		} else {
			task.ClosedAt = nil
		}
	}
	if patch.Priority != nil {
		task.Priority = normalizePriority(*patch.Priority)
	}
	if patch.Assignee != nil {
		task.Assignee = strings.TrimSpace(*patch.Assignee)
	}
	if patch.ReplaceLabels {
		task.Labels = uniqueNonEmpty(patch.Labels)
	}
	if patch.Metadata != nil {
		task.Metadata = cloneRaw(*patch.Metadata)
	}
	task.UpdatedAt = now
}

func hasOpenBlocker(st *state, task Task) bool {
	for _, depID := range task.DependsOn {
		dep, ok := st.tasks[depID]
		if !ok || dep.Status != TaskStatusClosed {
			return true
		}
	}
	return false
}

func recomputeBlocks(st *state) {
	for id, task := range st.tasks {
		task.Blocks = nil
		st.tasks[id] = task
	}
	for id, task := range st.tasks {
		for _, depID := range task.DependsOn {
			dep, ok := st.tasks[depID]
			if !ok {
				continue
			}
			dep.Blocks = appendUnique(dep.Blocks, id)
			st.tasks[depID] = dep
		}
	}
}

func normalizeTaskType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TaskTypeBug:
		return TaskTypeBug
	case TaskTypeFeature, "feat":
		return TaskTypeFeature
	case TaskTypeChore:
		return TaskTypeChore
	case TaskTypeEpic:
		return TaskTypeEpic
	default:
		return TaskTypeTask
	}
}

func normalizeTaskStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TaskStatusInProgress, "in-progress", "claimed":
		return TaskStatusInProgress
	case TaskStatusBlocked:
		return TaskStatusBlocked
	case TaskStatusClosed, "done", "completed":
		return TaskStatusClosed
	case TaskStatusDeferred:
		return TaskStatusDeferred
	default:
		return TaskStatusOpen
	}
}

func normalizePriority(value int) int {
	if value < 0 {
		return 0
	}
	if value > 4 {
		return 4
	}
	return value
}

func normalizeMemoryKind(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case MemoryKindPinned:
		return MemoryKindPinned
	case MemoryKindEpisodic:
		return MemoryKindEpisodic
	case MemoryKindProcedural:
		return MemoryKindProcedural
	default:
		return MemoryKindSemantic
	}
}

func normalizeMemoryScope(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case MemoryScopeUser:
		return MemoryScopeUser
	case MemoryScopeTask:
		return MemoryScopeTask
	case MemoryScopeFile:
		return MemoryScopeFile
	default:
		return MemoryScopeProject
	}
}

func sanitizeProjectID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func newID(prefix string) string {
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	return prefix + "_" + id[:12]
}

func sortTasks(tasks []Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].Status != tasks[j].Status {
			return tasks[i].Status < tasks[j].Status
		}
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority < tasks[j].Priority
		}
		if !tasks[i].UpdatedAt.Equal(tasks[j].UpdatedAt) {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
}

func limitTasks(tasks []Task, limit int) []Task {
	if limit > 0 && len(tasks) > limit {
		tasks = tasks[:limit]
	}
	out := make([]Task, len(tasks))
	copy(out, tasks)
	return out
}

func memoryMatchesQuery(mem Memory, query string) bool {
	haystack := strings.ToLower(strings.Join(append([]string{mem.Content, mem.Kind, mem.Scope}, append(mem.Tags, append(mem.TaskIDs, mem.FilePaths...)...)...), " "))
	if strings.Contains(haystack, query) {
		return true
	}
	for _, term := range strings.Fields(query) {
		if strings.Contains(haystack, term) {
			return true
		}
	}
	return false
}

func matchesAny(actual string, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	actual = strings.ToLower(strings.TrimSpace(actual))
	for _, want := range wanted {
		if actual == strings.ToLower(strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func matchesLabels(actual, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(actual))
	for _, label := range actual {
		set[strings.ToLower(strings.TrimSpace(label))] = struct{}{}
	}
	for _, want := range wanted {
		if _, ok := set[strings.ToLower(strings.TrimSpace(want))]; !ok {
			return false
		}
	}
	return true
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return uniqueNonEmpty(values)
	}
	for _, existing := range values {
		if existing == value {
			return uniqueNonEmpty(values)
		}
	}
	return append(uniqueNonEmpty(values), value)
}

func removeString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	out := make([]string, 0, len(values))
	for _, existing := range values {
		if strings.TrimSpace(existing) != "" && existing != value {
			out = append(out, existing)
		}
	}
	return out
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func cloneTaskPtr(task Task) *Task {
	out := cloneTask(task)
	return &out
}

func cloneTask(task Task) Task {
	task.DependsOn = append([]string(nil), task.DependsOn...)
	task.Blocks = append([]string(nil), task.Blocks...)
	task.Labels = append([]string(nil), task.Labels...)
	task.Comments = append([]TaskComment(nil), task.Comments...)
	task.Metadata = cloneRaw(task.Metadata)
	if task.ClosedAt != nil {
		closed := *task.ClosedAt
		task.ClosedAt = &closed
	}
	return task
}

func cloneMemoryPtr(mem Memory) *Memory {
	out := cloneMemory(mem)
	return &out
}

func cloneMemory(mem Memory) Memory {
	mem.Tags = append([]string(nil), mem.Tags...)
	mem.TaskIDs = append([]string(nil), mem.TaskIDs...)
	mem.FilePaths = append([]string(nil), mem.FilePaths...)
	mem.Metadata = cloneRaw(mem.Metadata)
	if mem.LastReadAt != nil {
		t := *mem.LastReadAt
		mem.LastReadAt = &t
	}
	return mem
}

func cloneSession(summary SessionSummary) SessionSummary {
	summary.TaskIDs = append([]string(nil), summary.TaskIDs...)
	return summary
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
