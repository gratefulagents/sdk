package tracestore

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RunMetadata describes a single evaluation/observation run.
type RunMetadata struct {
	RunID          string    `json:"run_id"`
	CandidateID    string    `json:"candidate_id,omitempty"`
	Model          string    `json:"model,omitempty"`
	Mode           string    `json:"mode,omitempty"`
	PermissionMode string    `json:"permission_mode,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	MaxTurns       int       `json:"max_turns,omitempty"`
	Tools          []string  `json:"tools,omitempty"`
	McpServers     []string  `json:"mcp_servers,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
}

// RunFilter constrains which runs to list.
type RunFilter struct {
	CandidateID string
	Since       time.Time
}

// Score holds per-task evaluation results.
type Score struct {
	TaskID      string       `json:"task_id"`
	CandidateID string       `json:"candidate_id"`
	Success     bool         `json:"success"`
	Metrics     ScoreMetrics `json:"metrics"`
}

// ScoreMetrics captures quantitative evaluation data.
type ScoreMetrics struct {
	Accuracy       float64 `json:"accuracy"`
	TokensUsed     int64   `json:"tokens_used"`
	CostUSD        float64 `json:"cost_usd"`
	DurationSec    float64 `json:"duration_sec"`
	ToolCalls      int     `json:"tool_calls"`
	TurnsUsed      int     `json:"turns_used"`
	CompactionHits int     `json:"compaction_hits"`
}

// TraceStore persists execution traces so a proposer agent can browse them.
type TraceStore interface {
	// CreateRunDir initialises a run directory and writes metadata.json.
	CreateRunDir(runID string, metadata RunMetadata) (string, error)
	// AppendTrace appends a single NDJSON line to a category file (e.g. "llm_calls").
	AppendTrace(runID string, category string, data []byte) error
	// WriteFile writes arbitrary data to a path relative to the run directory.
	WriteFile(runID string, relPath string, data []byte) error
	// WriteScore writes score.json into the run directory.
	WriteScore(runID string, score Score) error
	// ListRuns returns metadata for runs matching the filter.
	ListRuns(filter RunFilter) ([]RunMetadata, error)
	// RunDir returns the absolute path to a run's directory.
	RunDir(runID string) (string, error)
	// UpdateMetadataFinishedAt updates the FinishedAt field in metadata.json.
	UpdateMetadataFinishedAt(runID string, finishedAt time.Time) error
	// UpdateMetadataMode updates the Mode field in metadata.json.
	UpdateMetadataMode(runID string, mode string) error
}

// FilesystemTraceStore is a TraceStore backed by local filesystem directories.
//
// Layout:
//
//	{root}/traces/{run-id}/
//	  metadata.json
//	  llm_calls.jsonl
//	  tool_calls.jsonl
//	  ...
type FilesystemTraceStore struct {
	root   string
	rootFD int
	mu     sync.Mutex
}

// NewFilesystemTraceStore creates a store rooted at the given directory.
func NewFilesystemTraceStore(root string) (*FilesystemTraceStore, error) {
	canonicalRoot, rootFD, err := initializeFilesystemRoot(root)
	if err != nil {
		return nil, err
	}
	return &FilesystemTraceStore{root: canonicalRoot, rootFD: rootFD}, nil
}

func (s *FilesystemTraceStore) tracesDir() string {
	return filepath.Join(s.root, "traces")
}

func (s *FilesystemTraceStore) runDir(runID string) (string, error) {
	safeRunID, err := safeTraceName("run id", runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.tracesDir(), safeRunID), nil
}

func (s *FilesystemTraceStore) CreateRunDir(runID string, metadata RunMetadata) (string, error) {
	if _, err := s.runDir(runID); err != nil {
		return "", err
	}
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	if err := s.createRunDir(runID, metaBytes); err != nil {
		return "", err
	}
	verifiedDir, err := s.RunDir(runID)
	if err != nil {
		return "", err
	}
	return verifiedDir, nil
}

func (s *FilesystemTraceStore) AppendTrace(runID string, category string, data []byte) error {
	if _, err := s.runDir(runID); err != nil {
		return err
	}
	safeCategory, err := safeTraceName("trace category", category)
	if err != nil {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return s.appendTrace(runID, safeCategory+".jsonl", data)
}

func (s *FilesystemTraceStore) WriteFile(runID string, relPath string, data []byte) error {
	if _, err := s.runDir(runID); err != nil {
		return err
	}
	safeRelPath, err := safeTraceRelPath(relPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return s.writeFile(runID, safeRelPath, data)
}

func (s *FilesystemTraceStore) WriteScore(runID string, score Score) error {
	data, err := json.MarshalIndent(score, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal score: %w", err)
	}
	return s.WriteFile(runID, "score.json", data)
}

func (s *FilesystemTraceStore) ListRuns(filter RunFilter) ([]RunMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	return s.listRuns(filter)
}

func (s *FilesystemTraceStore) RunDir(runID string) (string, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return "", err
	}
	if err := s.verifyRunDir(runID, dir); err != nil {
		return "", fmt.Errorf("run dir %s: %w", runID, err)
	}
	return dir, nil
}

// Close releases the descriptor pinning the trusted trace root. It is safe to
// call more than once; subsequent store operations fail closed.
func (s *FilesystemTraceStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rootFD < 0 {
		return nil
	}
	err := closeFilesystemRoot(s.rootFD)
	s.rootFD = -1
	return err
}

func (s *FilesystemTraceStore) ensureOpen() error {
	if s == nil || s.rootFD < 0 {
		return fmt.Errorf("filesystem trace store is closed")
	}
	return nil
}

// UpdateMetadataFinishedAt updates the FinishedAt field in metadata.json.
func (s *FilesystemTraceStore) UpdateMetadataFinishedAt(runID string, finishedAt time.Time) error {
	return s.updateMetadata(runID, func(m *RunMetadata) {
		m.FinishedAt = finishedAt
	})
}

// UpdateMetadataMode updates the Mode field in metadata.json.
func (s *FilesystemTraceStore) UpdateMetadataMode(runID string, mode string) error {
	return s.updateMetadata(runID, func(m *RunMetadata) {
		m.Mode = mode
	})
}

func (s *FilesystemTraceStore) updateMetadata(runID string, mutate func(*RunMetadata)) error {
	if _, err := s.runDir(runID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}

	data, err := s.readMetadata(runID)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	var meta RunMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}
	mutate(&meta)
	updated, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	return s.writeMetadata(runID, updated)
}

func safeTraceName(kind, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", kind)
	}
	if filepath.IsAbs(trimmed) || strings.ContainsAny(trimmed, `/\\`) {
		return "", fmt.Errorf("%s %q must be a single path segment", kind, value)
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || clean != trimmed {
		return "", fmt.Errorf("%s %q must be a clean path segment", kind, value)
	}
	return trimmed, nil
}

func safeTraceRelPath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("trace file path is required")
	}
	if filepath.IsAbs(trimmed) || strings.Contains(trimmed, `\`) {
		return "", fmt.Errorf("trace file path %q must be relative", value)
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("trace file path %q escapes the run directory", value)
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("trace file path %q contains an unsafe path segment", value)
		}
	}
	return clean, nil
}
