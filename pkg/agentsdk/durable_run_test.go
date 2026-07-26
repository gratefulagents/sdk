package agentsdk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/durable"
)

func TestStoredRunLeaseCheckpointAndResume(t *testing.T) {
	ctx := context.Background()
	store, err := durable.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := StoredRunOptions{TenantID: "tenant_a", RunID: "run_a", Owner: "worker_a", LeaseTTL: time.Minute}
	first, err := OpenStoredRun(ctx, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStoredRun(ctx, store, StoredRunOptions{TenantID: "tenant_a", RunID: "run_a", Owner: "worker_b", LeaseTTL: time.Minute}); !errors.Is(err, durable.ErrLeaseHeld) {
		t.Fatalf("second owner error = %v", err)
	}
	cfg := first.RunConfig()
	now := time.Now().UTC()
	cp := DurableCheckpoint{SchemaVersion: DurableCheckpointSchemaVersion, RunID: string(first.ID()), AttemptID: cfg.AttemptID, StepID: "step_a", Sequence: 1, Boundary: DurableBoundaryToolCompleted, AgentName: "worker", Usage: Usage{InputTokens: 11, OutputTokens: 7}, CreatedAt: now}
	if err := cfg.Checkpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	resumed, err := OpenStoredRun(ctx, store, StoredRunOptions{TenantID: "tenant_a", RunID: "run_a", Owner: "worker_b", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(ctx)
	resumeCfg := resumed.RunConfig()
	if resumeCfg.Resume == nil || resumeCfg.Resume.StepID != "step_a" || resumeCfg.Resume.Boundary != DurableBoundaryToolCompleted {
		t.Fatalf("resume checkpoint = %+v", resumeCfg.Resume)
	}
	second := DurableCheckpoint{SchemaVersion: DurableCheckpointSchemaVersion, RunID: "run_a", AttemptID: resumeCfg.AttemptID, StepID: "step_b", Sequence: 2, Boundary: DurableBoundaryPaused, Usage: Usage{InputTokens: 5, OutputTokens: 2}, CreatedAt: now.Add(time.Second)}
	if err := resumeCfg.Checkpoint(ctx, second); err != nil {
		t.Fatal(err)
	}
	snapshot, events, err := store.Load(ctx, "tenant_a", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 2 || snapshot.CumulativeBudget.InputTokens != 16 || snapshot.CumulativeBudget.OutputTokens != 9 || len(events) != 2 {
		t.Fatalf("snapshot=%+v events=%+v", snapshot, events)
	}
}
