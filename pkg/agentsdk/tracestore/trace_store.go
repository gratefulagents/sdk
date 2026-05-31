package tracestore

import (
	"encoding/json"
	"fmt"
	"os"
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
	root string
	mu   sync.Mutex
}

// NewFilesystemTraceStore creates a store rooted at the given directory.
func NewFilesystemTraceStore(root string) (*FilesystemTraceStore, error) {
	tracesDir := filepath.Join(root, "traces")
	if err := os.MkdirAll(tracesDir, 0o700); err != nil {
		return nil, fmt.Errorf("create traces dir: %w", err)
	}
	return &FilesystemTraceStore{root: root}, nil
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
	dir, err := s.runDir(runID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create run dir: %w", err)
	}
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), metaBytes, 0o600); err != nil {
		return "", fmt.Errorf("write metadata: %w", err)
	}
	return dir, nil
}

func (s *FilesystemTraceStore) AppendTrace(runID string, category string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.runDir(runID)
	if err != nil {
		return err
	}
	safeCategory, err := safeTraceName("trace category", category)
	if err != nil {
		return err
	}
	filePath := filepath.Join(dir, safeCategory+".jsonl")
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", category, err)
	}
	defer f.Close()

	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	_, err = f.Write(data)
	return err
}

func (s *FilesystemTraceStore) WriteFile(runID string, relPath string, data []byte) error {
	dir, err := s.runDir(runID)
	if err != nil {
		return err
	}
	safeRelPath, err := safeTraceRelPath(relPath)
	if err != nil {
		return err
	}
	full := filepath.Join(dir, safeRelPath)
	parentDir := filepath.Dir(full)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDir, err)
	}
	return os.WriteFile(full, data, 0o600)
}

func (s *FilesystemTraceStore) WriteScore(runID string, score Score) error {
	data, err := json.MarshalIndent(score, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal score: %w", err)
	}
	return s.WriteFile(runID, "score.json", data)
}

func (s *FilesystemTraceStore) ListRuns(filter RunFilter) ([]RunMetadata, error) {
	entries, err := os.ReadDir(s.tracesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read traces dir: %w", err)
	}

	var runs []RunMetadata
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metaPath := filepath.Join(s.tracesDir(), entry.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta RunMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if filter.CandidateID != "" && meta.CandidateID != filter.CandidateID {
			continue
		}
		if !filter.Since.IsZero() && meta.StartedAt.Before(filter.Since) {
			continue
		}
		runs = append(runs, meta)
	}
	return runs, nil
}

func (s *FilesystemTraceStore) RunDir(runID string) (string, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("run dir %s: %w", runID, err)
	}
	return dir, nil
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

// updateMetadata reads, mutates, and writes back metadata.json atomically.
func (s *FilesystemTraceStore) updateMetadata(runID string, mutate func(*RunMetadata)) error {
	dir, err := s.runDir(runID)
	if err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "metadata.json")

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(metaPath)
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
	return os.WriteFile(metaPath, updated, 0o600)
}

func safeTraceName(kind, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", kind)
	}
	if filepath.IsAbs(trimmed) || strings.ContainsAny(trimmed, `/\`) {
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
	if filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("trace file path %q must be relative", value)
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("trace file path %q escapes the run directory", value)
	}
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("trace file path %q contains an unsafe path segment", value)
		}
	}
	return clean, nil
}
