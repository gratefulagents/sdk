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

// FilesystemStore persists durable project state as an append-only events.jsonl
// log plus JSON index snapshots under a project state directory.
type FilesystemStore struct {
	*engine
	backend *fsBackend
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

	be := &fsBackend{stateDir: stateDir, embedder: opts.Embedder}
	hybrid := DefaultHybridConfig()
	if opts.Hybrid != nil {
		hybrid = *opts.Hybrid
	}
	eng := &engine{
		projectID: projectID,
		workDir:   workDir,
		actor:     strings.TrimSpace(opts.Actor),
		runID:     strings.TrimSpace(opts.RunID),
		stateDir:  stateDir,
		embedder:  opts.Embedder,
		hybrid:    hybrid,
		backend:   be,
	}
	store := &FilesystemStore{engine: eng, backend: be}
	if err := be.ensureDirs(); err != nil {
		return nil, err
	}
	if err := eng.initialize(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *FilesystemStore) StateDir() string { return s.engine.stateDir }

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

// fsBackend implements backend using an append-only events.jsonl log, JSON index
// snapshots, an embeddings.json vector cache, and a file-based advisory lock.
type fsBackend struct {
	stateDir string
	embedder Embedder
	embedMu  sync.Mutex
}

func (b *fsBackend) close() error { return nil }

func (b *fsBackend) lock(ctx context.Context) (func(), error) {
	if err := b.ensureDirs(); err != nil {
		return nil, err
	}
	return b.acquireLock(ctx)
}

func (b *fsBackend) ensureDirs() error {
	for _, dir := range []string{b.stateDir, filepath.Join(b.stateDir, "indexes"), filepath.Join(b.stateDir, "snapshots"), filepath.Join(b.stateDir, "locks")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func (b *fsBackend) acquireLock(ctx context.Context) (func(), error) {
	lockPath := filepath.Join(b.stateDir, "locks", "state.lock")
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

func (b *fsBackend) loadEvents() ([]Event, error) {
	path := filepath.Join(b.stateDir, eventsFileName)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open events: %w", err)
	}
	defer f.Close()
	var events []Event
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
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan events: %w", err)
	}
	return events, nil
}

func (b *fsBackend) appendEvent(ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	path := filepath.Join(b.stateDir, eventsFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events for append: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

func (b *fsBackend) snapshot(st *state) error {
	now := time.Now().UTC()
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

	if err := writeJSONAtomic(filepath.Join(b.stateDir, "indexes", "project.json"), st.project); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(b.stateDir, "indexes", "tasks.json"), tasksIndex{SchemaVersion: SchemaVersion, UpdatedAt: now, Tasks: tasks}); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(b.stateDir, "indexes", "memories.json"), memoriesIndex{SchemaVersion: SchemaVersion, UpdatedAt: now, Memories: memories}); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(b.stateDir, "indexes", "sessions.json"), sessionsIndex{SchemaVersion: SchemaVersion, UpdatedAt: now, Sessions: sessions}); err != nil {
		return err
	}
	return nil
}

// readEmbeddings loads the vector cache from indexes/embeddings.json. A missing
// file yields an empty cache rather than an error.
func (b *fsBackend) readEmbeddings() (map[string]embeddingRecord, error) {
	b.embedMu.Lock()
	defer b.embedMu.Unlock()
	path := filepath.Join(b.stateDir, "indexes", embeddingsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]embeddingRecord{}, nil
		}
		return nil, err
	}
	var cache embeddingCache
	if err := json.Unmarshal(data, &cache); err != nil {
		// A corrupt derived cache should not break recall; rebuild lazily.
		return map[string]embeddingRecord{}, nil
	}
	if cache.Vectors == nil {
		cache.Vectors = map[string]embeddingRecord{}
	}
	return cache.Vectors, nil
}

// writeEmbeddings atomically persists the vector cache.
func (b *fsBackend) writeEmbeddings(records map[string]embeddingRecord, model string) error {
	b.embedMu.Lock()
	defer b.embedMu.Unlock()
	cache := embeddingCache{
		SchemaVersion: SchemaVersion,
		Model:         model,
		UpdatedAt:     time.Now().UTC(),
		Vectors:       records,
	}
	return writeJSONAtomic(filepath.Join(b.stateDir, "indexes", embeddingsFileName), cache)
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
