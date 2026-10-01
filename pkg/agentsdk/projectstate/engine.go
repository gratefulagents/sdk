package projectstate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// embedWriteTimeout bounds the best-effort embedding call on the memory write
// path so an unreachable provider cannot stall SaveMemory for the embedder's
// full HTTP timeout.
const embedWriteTimeout = 5 * time.Second

// backend abstracts durable persistence for the projectstate engine. The engine
// keeps all storage-agnostic logic (event sourcing, task/memory semantics,
// hybrid recall, priming) and delegates only the read/write of the
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
	// upsertEmbeddings merges the given records into the durable vector cache
	// without clobbering records written concurrently by other callers.
	upsertEmbeddings(records map[string]embeddingRecord, model string) error
	// deleteEmbeddings removes cached vectors for the given memory ids.
	deleteEmbeddings(ids []string) error
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
			Type:        NormalizeTaskType(in.Type),
			Status:      TaskStatusOpen,
			Priority:    NormalizePriority(in.Priority),
			Assignee:    strings.TrimSpace(in.Assignee),
			DependsOn:   uniqueNonEmpty(in.DependsOn),
			Labels:      uniqueNonEmpty(in.Labels),
			CreatedAt:   now,
			UpdatedAt:   now,
			SourceRun:   firstNonEmpty(in.SourceRun, s.runID),
			Metadata:    cloneRaw(in.Metadata),
		}
		st.tasks[task.ID] = task
		RecomputeBlocks(st.tasks)
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
		ApplyTaskPatch(&task, patch, now)
		st.tasks[task.ID] = task
		RecomputeBlocks(st.tasks)
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
		RecomputeBlocks(st.tasks)
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
		RecomputeBlocks(st.tasks)
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
		tasks = append(tasks, task)
	}
	return ReadyFromTasks(tasks, filter), nil
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
	SortTasks(tasks)
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
		RecomputeBlocks(st.tasks)
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
		RecomputeBlocks(st.tasks)
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

func (s *engine) ReleaseClaims(ctx context.Context, actor, note string) ([]Task, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, fmt.Errorf("actor is required")
	}
	note = strings.TrimSpace(note)
	var out []Task
	err := s.mutate(ctx, "task.claims_released", func(st *state, now time.Time) (any, error) {
		var released []Task
		for _, task := range st.tasks {
			if task.Status != TaskStatusInProgress || task.Assignee != actor {
				continue
			}
			task.Status = TaskStatusOpen
			task.Assignee = ""
			task.ClosedAt = nil
			task.UpdatedAt = now
			if note != "" {
				task.Comments = append(task.Comments, TaskComment{ID: newID("comment"), Actor: actor, Body: note, CreatedAt: now})
			}
			st.tasks[task.ID] = task
			released = append(released, cloneTask(task))
		}
		if len(released) == 0 {
			return nil, nil
		}
		SortTasks(released)
		out = released
		return claimsReleasedPayload{Actor: actor, Note: note, At: now, Tasks: released}, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *engine) SaveMemory(ctx context.Context, in SaveMemoryInput) (*Memory, error) {
	if err := ValidateMemoryInput(&in); err != nil {
		return nil, err
	}
	var out *Memory
	err := s.mutate(ctx, "memory.saved", func(st *state, now time.Time) (any, error) {
		mem := Memory{
			ID:         in.ID,
			Kind:       in.Kind,
			Title:      in.Title,
			Body:       in.Body,
			Citations:  in.Citations,
			CommitSHA:  in.CommitSHA,
			SourceRun:  firstNonEmpty(in.SourceRun, s.runID),
			CreatedAt:  now,
			UpdatedAt:  now,
			VerifiedAt: now,
		}
		if mem.ID != "" {
			existing, ok := st.memories[mem.ID]
			if !ok {
				return nil, fmt.Errorf("memory %q not found; omit id to create a new memory", mem.ID)
			}
			mem.CreatedAt = existing.CreatedAt
			mem.UseCount = existing.UseCount
			mem.LastUsedAt = existing.LastUsedAt
			// SourceRun records the creator; later editors (including the
			// consolidator) must not take ownership of shared memories.
			mem.SourceRun = firstNonEmpty(existing.SourceRun, mem.SourceRun)
		} else {
			if len(st.memories) >= DefaultMemoryCap {
				return nil, fmt.Errorf("project already has %d memories (cap %d). Consolidate instead: update an existing memory with memory_save and its id, or remove obsolete ones with memory_delete", len(st.memories), DefaultMemoryCap)
			}
			mem.ID = newID("mem")
		}
		st.memories[mem.ID] = mem
		out = cloneMemoryPtr(mem)
		return mem, nil
	})
	if err != nil {
		return nil, err
	}
	// Best-effort: caching the embedding must never fail the write. A missing
	// vector is backfilled lazily on the next search. Bound the embedding call
	// so a dead provider cannot stall the write path for the full HTTP timeout.
	embedCtx, cancel := context.WithTimeout(ctx, embedWriteTimeout)
	if err := s.cacheMemoryEmbedding(embedCtx, out.ID, memoryEmbedText(*out)); err != nil {
		log.Printf("projectstate: caching embedding for memory %s failed (will backfill on search): %v", out.ID, err)
	}
	cancel()
	return out, nil
}

func (s *engine) GetMemory(ctx context.Context, id string) (*Memory, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	mem, ok := st.memories[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("memory %q not found", id)
	}
	return cloneMemoryPtr(mem), nil
}

// SearchMemories ranks memories by LexicalScore and, when an Embedder is
// configured, fuses in cosine similarity over cached embeddings.
func (s *engine) SearchMemories(ctx context.Context, q MemoryQuery) ([]MemoryHit, error) {
	query := strings.TrimSpace(q.Query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	candidates := filterMemoryKinds(st.memories, q.Kinds)
	if len(candidates) == 0 {
		return nil, nil
	}
	var queryVec []float32
	var vectors map[string][]float32
	if s.embedder != nil {
		// Without a query vector there is no semantic signal; rank lexically
		// rather than failing the search.
		if vecs, err := s.embedder.Embed(ctx, []string{query}); err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			queryVec = vecs[0]
			vectors = s.ensureEmbeddings(ctx, candidates)
		}
	}
	hits := rankHybrid(query, candidates, queryVec, vectors, s.hybrid, time.Now().UTC())
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (s *engine) ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return nil, err
	}
	out := filterMemoryKinds(st.memories, filter.Kinds)
	sortMemories(out)
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *engine) DeleteMemory(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	err := s.mutate(ctx, "memory.deleted", func(st *state, now time.Time) (any, error) {
		if _, ok := st.memories[id]; !ok {
			return nil, fmt.Errorf("memory %q not found", id)
		}
		delete(st.memories, id)
		return memoryDeletedPayload{ID: id, At: now}, nil
	})
	if err != nil {
		return err
	}
	// Best-effort cache prune; a leftover orphan vector only wastes space.
	_ = s.backend.deleteEmbeddings([]string{id})
	return nil
}

func (s *engine) VerifyMemory(ctx context.Context, id, commitSHA string) (*Memory, error) {
	id = strings.TrimSpace(id)
	commitSHA = strings.TrimSpace(commitSHA)
	var out *Memory
	err := s.mutate(ctx, "memory.verified", func(st *state, now time.Time) (any, error) {
		mem, ok := st.memories[id]
		if !ok {
			return nil, fmt.Errorf("memory %q not found", id)
		}
		p := memoryVerifiedPayload{ID: id, CommitSHA: commitSHA, At: now}
		applyMemoryVerified(&mem, p)
		st.memories[id] = mem
		out = cloneMemoryPtr(mem)
		return p, nil
	})
	return out, err
}

func (s *engine) TouchMemories(ctx context.Context, ids []string) error {
	return s.mutate(ctx, "memory.touched", func(st *state, now time.Time) (any, error) {
		var known []string
		for _, id := range uniqueNonEmpty(ids) {
			if _, ok := st.memories[id]; ok {
				known = append(known, id)
			}
		}
		if len(known) == 0 {
			return nil, nil
		}
		p := memoryTouchedPayload{IDs: known, At: now}
		applyMemoryTouched(st, p)
		return p, nil
	})
}

func filterMemoryKinds(memories map[string]Memory, kinds []string) []Memory {
	wanted := map[string]bool{}
	for _, kind := range kinds {
		if strings.TrimSpace(kind) != "" {
			wanted[NormalizeMemoryKind(kind)] = true
		}
	}
	out := make([]Memory, 0, len(memories))
	for _, mem := range memories {
		if len(wanted) > 0 && !wanted[mem.Kind] {
			continue
		}
		out = append(out, cloneMemory(mem))
	}
	return out
}

var memoryKindRank = map[string]int{
	MemoryKindPreference: 0,
	MemoryKindDecision:   1,
	MemoryKindProcedure:  2,
	MemoryKindFact:       3,
}

// sortMemories orders by kind priority (preference, decision, procedure,
// fact), then UpdatedAt desc, then ID.
func sortMemories(memories []Memory) {
	sort.SliceStable(memories, func(i, j int) bool {
		ri, rj := memoryKindRank[memories[i].Kind], memoryKindRank[memories[j].Kind]
		if ri != rj {
			return ri < rj
		}
		if !memories[i].UpdatedAt.Equal(memories[j].UpdatedAt) {
			return memories[i].UpdatedAt.After(memories[j].UpdatedAt)
		}
		return memories[i].ID < memories[j].ID
	})
}

func memoryEmbedText(mem Memory) string {
	return mem.Title + "\n" + mem.Body
}

func (s *engine) mutate(ctx context.Context, eventType string, fn func(*state, time.Time) (any, error)) error {
	return s.withLockedState(ctx, func(st *state) error {
		now := time.Now().UTC()
		payload, err := fn(st, now)
		if err != nil {
			return err
		}
		if payload == nil {
			return nil
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
	RecomputeBlocks(st.tasks)
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

// PrimeContext renders the durable project state with RenderBriefing.
// MemoryLimit is not used: the memory index is bounded by BriefingMemoryBudget.
func (s *engine) PrimeContext(ctx context.Context, opts PrimeOptions) (string, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return "", err
	}
	opts.Actor = firstNonEmpty(opts.Actor, s.actor)
	if opts.ReadyLimit <= 0 {
		opts.ReadyLimit = 8
	}
	tasks := make([]Task, 0, len(st.tasks))
	for _, task := range st.tasks {
		tasks = append(tasks, task)
	}
	memories := make([]Memory, 0, len(st.memories))
	for _, mem := range st.memories {
		memories = append(memories, mem)
	}
	return RenderBriefing(BriefingInput{
		ProjectID: st.project.ProjectID,
		Active:    ActiveTask(tasks, opts.ActiveTaskID, opts.Actor),
		Ready:     ReadyFromTasks(tasks, TaskFilter{Actor: opts.Actor, Limit: opts.ReadyLimit}),
		Blocked:   BlockedFromTasks(tasks, 5),
		Memories:  memories,
	}), nil
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
	case "memory.saved":
		var mem Memory
		if err := json.Unmarshal(ev.Payload, &mem); err != nil {
			return err
		}
		st.memories[mem.ID] = mem
	case "memory.upserted":
		var legacy legacyMemory
		if err := json.Unmarshal(ev.Payload, &legacy); err != nil {
			return err
		}
		mem := legacy.toMemory()
		st.memories[mem.ID] = mem
	case "memory.verified":
		var p memoryVerifiedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		if mem, ok := st.memories[p.ID]; ok {
			applyMemoryVerified(&mem, p)
			st.memories[p.ID] = mem
		}
	case "memory.touched":
		var p memoryTouchedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		applyMemoryTouched(st, p)
	case "memory.deleted":
		var p memoryDeletedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		delete(st.memories, p.ID)
	case "task.claims_released":
		var p claimsReleasedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		for _, task := range p.Tasks {
			st.tasks[task.ID] = task
		}
	case "session.summary_saved":
		// v1 session summaries were never read back; v2 drops them.
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

type memoryVerifiedPayload struct {
	ID        string    `json:"id"`
	CommitSHA string    `json:"commit_sha,omitempty"`
	At        time.Time `json:"at"`
}

type memoryTouchedPayload struct {
	IDs []string  `json:"ids"`
	At  time.Time `json:"at"`
}

type claimsReleasedPayload struct {
	Actor string    `json:"actor"`
	Note  string    `json:"note,omitempty"`
	At    time.Time `json:"at"`
	Tasks []Task    `json:"tasks"`
}

// legacyMemory is the v1 memory.upserted payload.
type legacyMemory struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	FilePaths []string  `json:"file_paths"`
	SourceRun string    `json:"source_run"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (l legacyMemory) toMemory() Memory {
	mem := Memory{
		ID:         l.ID,
		Kind:       NormalizeMemoryKind(l.Kind),
		Title:      LegacyMemoryTitle(l.Content),
		Body:       strings.TrimSpace(l.Content),
		SourceRun:  l.SourceRun,
		CreatedAt:  l.CreatedAt,
		UpdatedAt:  l.UpdatedAt,
		VerifiedAt: l.UpdatedAt,
	}
	for _, path := range uniqueNonEmpty(l.FilePaths) {
		mem.Citations = append(mem.Citations, Citation{Path: path})
	}
	return mem
}

func applyMemoryVerified(mem *Memory, p memoryVerifiedPayload) {
	mem.VerifiedAt = p.At
	if p.CommitSHA != "" {
		mem.CommitSHA = p.CommitSHA
	}
}

func applyMemoryTouched(st *state, p memoryTouchedPayload) {
	for _, id := range p.IDs {
		mem, ok := st.memories[id]
		if !ok {
			continue
		}
		at := p.At
		mem.UseCount++
		mem.LastUsedAt = &at
		st.memories[id] = mem
	}
}

// readEmbeddings loads the cached vectors via the backend.
func (s *engine) readEmbeddings() (map[string]embeddingRecord, error) {
	return s.backend.readEmbeddings()
}

// upsertEmbeddings merges the given records into the durable cache via the
// backend. Callers pass only the records they changed so concurrent writers
// (upsert path vs. recall backfill) cannot clobber each other's vectors.
func (s *engine) upsertEmbeddings(records map[string]embeddingRecord) error {
	if len(records) == 0 {
		return nil
	}
	model := ""
	if s.embedder != nil {
		model = s.embedder.Model()
	}
	return s.backend.upsertEmbeddings(records, model)
}

// cacheMemoryEmbedding embeds a single memory's text and persists it. It is
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
	return s.upsertEmbeddings(map[string]embeddingRecord{id: {
		Hash:   hashContent(content),
		Model:  s.embedder.Model(),
		Dims:   len(vecs[0]),
		Vector: vecs[0],
	}})
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
		text := memoryEmbedText(mem)
		if rec, ok := records[mem.ID]; ok && rec.fresh(text, model) {
			vectors[mem.ID] = rec.Vector
			continue
		}
		missingIDs = append(missingIDs, mem.ID)
		missingText = append(missingText, text)
	}

	if len(missingText) == 0 {
		return vectors
	}
	vecs, err := s.embedder.Embed(ctx, missingText)
	if err != nil || len(vecs) != len(missingText) {
		// Backfill failed; return whatever we already had cached.
		return vectors
	}
	refreshed := make(map[string]embeddingRecord, len(missingIDs))
	for i, id := range missingIDs {
		if len(vecs[i]) == 0 {
			continue
		}
		refreshed[id] = embeddingRecord{
			Hash:   hashContent(missingText[i]),
			Model:  model,
			Dims:   len(vecs[i]),
			Vector: vecs[i],
		}
		vectors[id] = vecs[i]
	}
	if err := s.upsertEmbeddings(refreshed); err != nil {
		log.Printf("projectstate: persisting backfilled embeddings failed: %v", err)
	}
	return vectors
}
