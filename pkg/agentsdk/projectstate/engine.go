package projectstate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// backend abstracts durable persistence for the projectstate engine. The engine
// keeps all storage-agnostic logic (event sourcing, task/memory/session
// semantics, hybrid recall, priming) and delegates only the read/write of the
// durable event log, derived snapshots, and the embedding cache to a backend.
//
// Two backends are provided: a filesystem backend (append-only events.jsonl plus
// JSON index snapshots) and a SQLite backend (events table; snapshots are a
// no-op because state is always rebuilt from the events table).
type backend interface {
	// loadEvents returns every durable event in ascending seq order. The engine
	// rebuilds the in-memory state by replaying these through applyEvent.
	loadEvents() ([]Event, error)
	// appendEvent durably persists one already seq-assigned event.
	appendEvent(ev Event) error
	// snapshot optionally writes derived index snapshots. May be a no-op.
	snapshot(st *state) error
	// lock serializes access (cross-process where supported) and returns a
	// release function.
	lock(ctx context.Context) (func(), error)
	// readEmbeddings loads the cached vectors keyed by memory id.
	readEmbeddings() (map[string]embeddingRecord, error)
	// writeEmbeddings persists the full vector cache.
	writeEmbeddings(records map[string]embeddingRecord, model string) error
	// close releases backend resources.
	close() error
}

// engine holds all storage-agnostic projectstate logic shared by every backend.
type engine struct {
	mu        sync.Mutex
	projectID string
	workDir   string
	actor     string
	runID     string
	stateDir  string
	embedder  Embedder
	hybrid    HybridConfig
	backend   backend
}

type state struct {
	project  Project
	tasks    map[string]Task
	memories map[string]Memory
	sessions map[string]SessionSummary
	lastSeq  int64
}

// initialize records the project.initialized event on first use and writes the
// initial snapshot. It is idempotent across opens.
func (s *engine) initialize(ctx context.Context) error {
	return s.withLockedState(ctx, func(st *state) error {
		now := time.Now().UTC()
		if st.project.ProjectID == "" {
			st.project = Project{
				SchemaVersion: SchemaVersion,
				ProjectID:     s.projectID,
				WorkDir:       s.workDir,
				StateDir:      s.stateDir,
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
		return s.snapshotLocked(st)
	})
}

func (s *engine) Close() error { return s.backend.close() }

func (s *engine) ProjectID() string { return s.projectID }

func (s *engine) CreateTask(ctx context.Context, in CreateTaskInput) (*Task, error) {
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

func (s *engine) UpdateTask(ctx context.Context, id string, patch TaskPatch) (*Task, error) {
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

func (s *engine) ClaimTask(ctx context.Context, id, actor string) (*Task, error) {
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

func (s *engine) CloseTask(ctx context.Context, id, reason string) (*Task, error) {
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

func (s *engine) ReadyTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
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

func (s *engine) ListTasks(ctx context.Context) ([]Task, error) {
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

func (s *engine) GetTask(ctx context.Context, id string) (*Task, error) {
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

func (s *engine) AddDependency(ctx context.Context, taskID, dependsOnID string) error {
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

func (s *engine) RemoveDependency(ctx context.Context, taskID, dependsOnID string) error {
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

func (s *engine) AddComment(ctx context.Context, taskID, actor, body string) (*TaskComment, error) {
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

func (s *engine) UpsertMemory(ctx context.Context, in UpsertMemoryInput) (*Memory, error) {
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

func (s *engine) SearchMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
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
func (s *engine) searchHybrid(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	// Embed the query first. If the embedding provider is unavailable or times
	// out we have no semantic signal, so fall back to the lexical search rather
	// than ranking every kind/tag match by recency and pinned boosts (which
	// would return query-irrelevant memories).
	vecs, embErr := s.embedder.Embed(ctx, []string{filter.Query})
	if embErr != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		return s.listMemories(ctx, filter, true)
	}
	queryVec := vecs[0]

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
	ranked := rankHybrid(filter.Query, candidates, queryVec, vectors, s.hybrid, time.Now().UTC())
	if filter.Limit > 0 && len(ranked) > filter.Limit {
		ranked = ranked[:filter.Limit]
	}
	return ranked, nil
}

func (s *engine) ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	return s.listMemories(ctx, filter, false)
}

func (s *engine) DeleteMemory(ctx context.Context, id string) error {
	return s.mutate(ctx, "memory.deleted", func(st *state, now time.Time) (any, error) {
		id = strings.TrimSpace(id)
		if _, ok := st.memories[id]; !ok {
			return nil, fmt.Errorf("memory %q not found", id)
		}
		delete(st.memories, id)
		return memoryDeletedPayload{ID: id, At: now}, nil
	})
}

func (s *engine) SaveSessionSummary(ctx context.Context, summary SessionSummary) (*SessionSummary, error) {
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

func (s *engine) ListSessionSummaries(ctx context.Context, limit int) ([]SessionSummary, error) {
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

func (s *engine) listMemories(ctx context.Context, filter MemoryFilter, requireQuery bool) ([]Memory, error) {
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

func (s *engine) mutate(ctx context.Context, eventType string, fn func(*state, time.Time) (any, error)) error {
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
		return s.snapshotLocked(st)
	})
}

func (s *engine) loadState(ctx context.Context) (*state, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.backend.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.loadStateLocked()
}

func (s *engine) withLockedState(ctx context.Context, fn func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.backend.lock(ctx)
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

func (s *engine) loadStateLocked() (*state, error) {
	st := &state{
		tasks:    make(map[string]Task),
		memories: make(map[string]Memory),
		sessions: make(map[string]SessionSummary),
	}
	events, err := s.backend.loadEvents()
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if ev.Seq > st.lastSeq {
			st.lastSeq = ev.Seq
		}
		if err := applyEvent(st, ev); err != nil {
			return nil, fmt.Errorf("apply event %d %s: %w", ev.Seq, ev.Type, err)
		}
	}
	recomputeBlocks(st)
	return st, nil
}

func (s *engine) appendEventLocked(st *state, eventType string, payload any, now time.Time) error {
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
	if err := s.backend.appendEvent(ev); err != nil {
		return err
	}
	st.lastSeq = ev.Seq
	return nil
}

// snapshotLocked normalizes the project record and asks the backend to persist
// any derived snapshots.
func (s *engine) snapshotLocked(st *state) error {
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
	return s.backend.snapshot(st)
}

// PrimeContext renders the durable project state into a compact briefing block.
func (s *engine) PrimeContext(ctx context.Context, opts PrimeOptions) (string, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return "", err
	}
	opts.Actor = firstNonEmpty(opts.Actor, s.actor)
	if opts.ReadyLimit <= 0 {
		opts.ReadyLimit = 8
	}
	if opts.MemoryLimit <= 0 {
		opts.MemoryLimit = 8
	}

	var b strings.Builder
	b.WriteString("## Durable Project State\n")
	if st.project.ProjectID != "" {
		b.WriteString("Project: " + st.project.ProjectID + "\n")
	}
	if st.project.WorkDir != "" {
		b.WriteString("Workspace: " + st.project.WorkDir + "\n")
	}

	active := activeTask(st, opts)
	if active != nil {
		b.WriteString("\n### Active Task\n")
		writeTaskLine(&b, *active)
		if active.Description != "" {
			b.WriteString("  " + oneLine(active.Description, 220) + "\n")
		}
		if len(active.DependsOn) > 0 {
			b.WriteString("  Depends on: " + strings.Join(active.DependsOn, ", ") + "\n")
		}
	}

	ready := readyFromState(st, TaskFilter{Actor: opts.Actor, Limit: opts.ReadyLimit})
	if len(ready) > 0 {
		b.WriteString("\n### Ready Work\n")
		for _, task := range ready {
			writeTaskLine(&b, task)
		}
	}

	blocked := blockedTasks(st, 5)
	if len(blocked) > 0 {
		b.WriteString("\n### Blocked Work\n")
		for _, task := range blocked {
			writeTaskLine(&b, task)
		}
	}

	pinned, recent := memoriesForPrime(st, opts.MemoryLimit)
	if len(pinned) > 0 {
		b.WriteString("\n### Pinned Memories\n")
		for _, mem := range pinned {
			b.WriteString("- " + oneLine(mem.Content, 220) + memorySuffix(mem) + "\n")
		}
	}
	if len(recent) > 0 {
		b.WriteString("\n### Recent Memories\n")
		for _, mem := range recent {
			b.WriteString("- " + oneLine(mem.Content, 180) + memorySuffix(mem) + "\n")
		}
	}

	out := strings.TrimSpace(b.String())
	if out == "## Durable Project State" {
		out += "\nNo durable tasks or memories yet."
	}
	return out, nil
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

// readEmbeddings loads the cached vectors via the backend.
func (s *engine) readEmbeddings() (map[string]embeddingRecord, error) {
return s.backend.readEmbeddings()
}

// writeEmbeddings persists the cached vectors via the backend.
func (s *engine) writeEmbeddings(records map[string]embeddingRecord) error {
model := ""
if s.embedder != nil {
model = s.embedder.Model()
}
return s.backend.writeEmbeddings(records, model)
}

// cacheMemoryEmbedding embeds a single memory's content and persists it. It is
// best-effort: embedding failures are returned but callers on the write path
// ignore them so storing a memory never fails because the embedder is down.
func (s *engine) cacheMemoryEmbedding(ctx context.Context, id, content string) error {
if s.embedder == nil || id == "" || content == "" {
return nil
}
records, err := s.readEmbeddings()
if err != nil {
return err
}
if rec, ok := records[id]; ok && rec.fresh(content, s.embedder.Model()) {
return nil
}
vecs, err := s.embedder.Embed(ctx, []string{content})
if err != nil {
return err
}
if len(vecs) != 1 || len(vecs[0]) == 0 {
return nil
}
records[id] = embeddingRecord{
Hash:   hashContent(content),
Model:  s.embedder.Model(),
Dims:   len(vecs[0]),
Vector: vecs[0],
}
return s.writeEmbeddings(records)
}

// ensureEmbeddings returns a memoryID -> vector map for the given memories,
// embedding any that are missing or stale and persisting the refreshed cache.
// Backfill failures degrade gracefully: whatever vectors already exist are
// returned so recall can still use them alongside the lexical signal.
func (s *engine) ensureEmbeddings(ctx context.Context, memories []Memory) map[string][]float32 {
vectors := map[string][]float32{}
if s.embedder == nil {
return vectors
}
records, err := s.readEmbeddings()
if err != nil {
records = map[string]embeddingRecord{}
}
model := s.embedder.Model()

var missingIDs []string
var missingText []string
for _, mem := range memories {
if rec, ok := records[mem.ID]; ok && rec.fresh(mem.Content, model) {
vectors[mem.ID] = rec.Vector
continue
}
if mem.Content == "" {
continue
}
missingIDs = append(missingIDs, mem.ID)
missingText = append(missingText, mem.Content)
}

if len(missingText) == 0 {
return vectors
}
vecs, err := s.embedder.Embed(ctx, missingText)
if err != nil || len(vecs) != len(missingText) {
// Backfill failed; return whatever we already had cached.
return vectors
}
for i, id := range missingIDs {
if len(vecs[i]) == 0 {
continue
}
records[id] = embeddingRecord{
Hash:   hashContent(missingText[i]),
Model:  model,
Dims:   len(vecs[i]),
Vector: vecs[i],
}
vectors[id] = vecs[i]
}
_ = s.writeEmbeddings(records)
return vectors
}
