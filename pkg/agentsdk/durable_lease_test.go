package agentsdk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/durable"
)

type renewalObservingStore struct {
	durable.RunStore
	renewed chan struct{}
}

func (s *renewalObservingStore) RenewLease(ctx context.Context, lease durable.Lease, ttl time.Duration) (durable.Lease, error) {
	renewed, err := s.RunStore.RenewLease(ctx, lease, ttl)
	if err == nil {
		select {
		case s.renewed <- struct{}{}:
		default:
		}
	}
	return renewed, err
}

func TestStoredRunRenewsLeaseDuringLongBoundary(t *testing.T) {
	ctx := context.Background()
	filesystem, err := durable.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &renewalObservingStore{RunStore: filesystem, renewed: make(chan struct{}, 8)}
	run, err := OpenStoredRun(ctx, store, StoredRunOptions{TenantID: "tenant_a", RunID: "run_a", Owner: "worker_a", LeaseTTL: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close(ctx)
	for i := 0; i < 3; i++ {
		select {
		case <-store.renewed:
		case <-time.After(10 * time.Second):
			// Generous bound: renewals normally arrive every few tens of
			// milliseconds, but loaded CI runners can stall goroutine
			// scheduling well past a second.
			t.Fatal("lease heartbeat did not renew")
		}
	}
	other, err := OpenStoredRun(ctx, store, StoredRunOptions{TenantID: "tenant_a", RunID: "run_a", Owner: "worker_b", LeaseTTL: time.Second})
	if other != nil || !errors.Is(err, durable.ErrLeaseHeld) {
		t.Fatalf("other=%v err=%v", other, err)
	}
}
