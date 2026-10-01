package agentsdk

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/durable"
)

type renewCountingStore struct {
	durable.RunStore
	renewals atomic.Int32
}

func (s *renewCountingStore) RenewLease(ctx context.Context, lease durable.Lease, ttl time.Duration) (durable.Lease, error) {
	s.renewals.Add(1)
	return s.RunStore.RenewLease(ctx, lease, ttl)
}

func openTestStoredRun(t *testing.T, store durable.RunStore, owner string) *StoredRun {
	t.Helper()
	run, err := OpenStoredRun(context.Background(), store, StoredRunOptions{TenantID: "tenant_a", RunID: "run_a", Owner: owner, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func testCheckpoint(cfg *DurableRunConfig, seq uint64, boundary DurableBoundary, in, out int64, at time.Time) DurableCheckpoint {
	return DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion, RunID: cfg.RunID, AttemptID: cfg.AttemptID,
		StepID: "step", Sequence: seq, Boundary: boundary, AgentName: "worker",
		History: []LLMRunItemSnapshot{{Type: "message", MessageText: "hello"}},
		Usage:   Usage{InputTokens: in, OutputTokens: out}, CreatedAt: at,
	}
}

func TestStoredRunUsageNotDoubleCountedAcrossResumes(t *testing.T) {
	ctx := context.Background()
	fs, err := durable.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	first := openTestStoredRun(t, fs, "worker_a")
	cfg := first.RunConfig()
	for i, in := range []int64{0, 600, 1000} {
		if err := cfg.Checkpoint(ctx, testCheckpoint(cfg, uint64(i+1), DurableBoundaryToolCompleted, in, in/10, now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// The runner restores Resume.Usage, so resumed checkpoints already carry
	// the earlier attempts' tokens.
	for attempt, end := range []int64{1500, 2200} {
		run := openTestStoredRun(t, fs, "worker_b")
		cfg := run.RunConfig()
		if cfg.Resume == nil {
			t.Fatal("expected resume checkpoint")
		}
		start := cfg.Resume.Usage.InputTokens
		for i, in := range []int64{start, end} {
			if err := cfg.Checkpoint(ctx, testCheckpoint(cfg, cfg.Resume.Sequence+uint64(i+1), DurableBoundaryToolCompleted, in, in/10, now)); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, _, err := fs.Load(ctx, "tenant_a", "run_a")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.CumulativeBudget.InputTokens != end || snapshot.CumulativeBudget.OutputTokens != end/10 {
			t.Fatalf("attempt %d cumulative = %+v, want input %d output %d", attempt, snapshot.CumulativeBudget, end, end/10)
		}
		if err := run.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoredRunUsageCountsRestartedCounters(t *testing.T) {
	ctx := context.Background()
	fs, err := durable.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := openTestStoredRun(t, fs, "worker_a")
	defer run.Close(ctx)
	cfg := run.RunConfig()
	now := time.Now().UTC()
	// Run reaches approval at 1000 tokens, the approved tool executes from a
	// zero counter, then the resumed Run counts 400 more from that checkpoint.
	for i, in := range []int64{1000, 0, 0, 400} {
		if err := cfg.Checkpoint(ctx, testCheckpoint(cfg, uint64(i+1), DurableBoundaryToolCompleted, in, 0, now)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _, err := fs.Load(ctx, "tenant_a", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CumulativeBudget.InputTokens != 1400 {
		t.Fatalf("cumulative input = %d, want 1400", snapshot.CumulativeBudget.InputTokens)
	}
}

func TestStoredRunCheckpointKeepsAttemptStartAndLightEvents(t *testing.T) {
	ctx := context.Background()
	fs, err := durable.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &renewCountingStore{RunStore: fs}
	run := openTestStoredRun(t, store, "worker_a")
	cfg := run.RunConfig()
	start := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := cfg.Checkpoint(ctx, testCheckpoint(cfg, uint64(i+1), DurableBoundaryToolCompleted, 0, 0, start.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.renewals.Load(); got != 0 {
		t.Fatalf("checkpoint renewed lease %d times; Append already validates the lease", got)
	}
	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}

	snapshot, events, err := fs.Load(ctx, "tenant_a", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Attempts) != 1 || !snapshot.Attempts[0].StartedAt.Equal(start) {
		t.Fatalf("attempts = %+v, want one attempt started at %v", snapshot.Attempts, start)
	}
	var state DurableCheckpoint
	if err := json.Unmarshal(snapshot.State, &state); err != nil || len(state.History) != 1 {
		t.Fatalf("snapshot state history = %+v err=%v", state.History, err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d", len(events))
	}
	for _, event := range events {
		var cp DurableCheckpoint
		if err := json.Unmarshal(event.Payload, &cp); err != nil {
			t.Fatal(err)
		}
		if len(cp.History) != 0 || cp.Boundary != DurableBoundaryToolCompleted {
			t.Fatalf("event payload carries history or lost boundary: %+v", cp)
		}
	}

	resumed := openTestStoredRun(t, fs, "worker_b")
	defer resumed.Close(ctx)
	next := resumed.RunConfig()
	if err := next.Checkpoint(ctx, testCheckpoint(next, 4, DurableBoundaryToolCompleted, 0, 0, start.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err = fs.Load(ctx, "tenant_a", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Attempts) != 2 || !snapshot.Attempts[0].StartedAt.Equal(start) || snapshot.Attempts[1].ID != durable.AttemptID(next.AttemptID) {
		t.Fatalf("attempts after resume = %+v", snapshot.Attempts)
	}
}

func TestCustomCheckpointImageStrippingDoesNotMutateCaller(t *testing.T) {
	images := []ImageAttachment{{MediaType: "image/png", Data: "private-image-payload"}}
	original := []LLMRunItemSnapshot{
		{Type: "message", MessageText: "picture", MessageImages: images},
		{Type: "tool_output", ToolOutput: &ToolOutputData{CallID: "read", Content: "screenshot", Images: images}},
	}
	got := stripCheckpointImages(original)
	if len(got[0].MessageImages) != 0 || len(got[1].ToolOutput.Images) != 0 {
		t.Fatal("checkpoint contains image attachments")
	}
	if got[0].MessageText != "picture\n[image omitted]" || got[1].ToolOutput.Content != "screenshot\n[image omitted]" {
		t.Fatal("missing image placeholders")
	}
	if len(original[0].MessageImages) != 1 || len(original[1].ToolOutput.Images) != 1 {
		t.Fatal("caller image attachments mutated")
	}
}
