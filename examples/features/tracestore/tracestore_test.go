package tracestore_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/tracestore"
)

func TestFilesystemTraceStoreExample(t *testing.T) {
	root := t.TempDir()
	store, err := tracestore.NewFilesystemTraceStore(root)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	now := time.Now().UTC()
	meta := tracestore.RunMetadata{
		RunID:          "run-1",
		CandidateID:    "cand-a",
		Model:          "gpt-5.5",
		Mode:           "exploration",
		PermissionMode: "workspace_write",
		MaxTurns:       8,
		Tools:          []string{"echo"},
		StartedAt:      now,
	}
	dir, err := store.CreateRunDir(meta.RunID, meta)
	if err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	if !strings.HasPrefix(dir, root) {
		t.Fatalf("run dir %q escapes root %q", dir, root)
	}
	if _, err := os.Stat(filepath.Join(dir, "metadata.json")); err != nil {
		t.Fatalf("metadata.json missing: %v", err)
	}

	llm := []byte(`{"event":"llm_call","tokens":42}`)
	if err := store.AppendTrace(meta.RunID, "llm_calls", llm); err != nil {
		t.Fatalf("append trace: %v", err)
	}
	if err := store.AppendTrace(meta.RunID, "llm_calls", []byte(`{"event":"llm_call","tokens":58}`)); err != nil {
		t.Fatalf("append trace 2: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "llm_calls.jsonl"))
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != 2 {
		t.Fatalf("ndjson lines = %d, want 2", lines)
	}

	score := tracestore.Score{
		TaskID:      "task-1",
		CandidateID: "cand-a",
		Success:     true,
		Metrics: tracestore.ScoreMetrics{
			Accuracy:    1.0,
			TokensUsed:  100,
			CostUSD:     0.0042,
			DurationSec: 1.23,
			ToolCalls:   1,
			TurnsUsed:   2,
		},
	}
	if err := store.WriteScore(meta.RunID, score); err != nil {
		t.Fatalf("write score: %v", err)
	}
	scoreRaw, err := os.ReadFile(filepath.Join(dir, "score.json"))
	if err != nil {
		t.Fatalf("read score: %v", err)
	}
	var roundtrip tracestore.Score
	if err := json.Unmarshal(scoreRaw, &roundtrip); err != nil {
		t.Fatalf("unmarshal score: %v", err)
	}
	if !roundtrip.Success || roundtrip.Metrics.TokensUsed != 100 {
		t.Fatalf("score roundtrip mismatch: %#v", roundtrip)
	}

	if err := store.UpdateMetadataFinishedAt(meta.RunID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("finish: %v", err)
	}

	all, err := store.ListRuns(tracestore.RunFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].RunID != meta.RunID {
		t.Fatalf("list = %#v", all)
	}

	none, err := store.ListRuns(tracestore.RunFilter{CandidateID: "does-not-exist"})
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected zero runs, got %d", len(none))
	}
}
