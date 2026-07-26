package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/durable"
)

// StoredRunOptions identifies and secures one durable execution. TenantID is
// required. Empty RunID creates a new stable run identity; empty Owner uses a
// process-scoped owner label. LeaseTTL defaults to 30 seconds.
type StoredRunOptions struct {
	TenantID       durable.TenantID
	RunID          durable.RunID
	Owner          string
	LeaseTTL       time.Duration
	Classification durable.DataClassification
	RetainUntil    *time.Time
}

// StoredRun binds Runner checkpoints to a RunStore lease and CAS revision.
// Close releases ownership; callers should always defer it.
type StoredRun struct {
	mu        sync.Mutex
	store     durable.RunStore
	snapshot  durable.RunSnapshot
	lease     durable.Lease
	leaseTTL  time.Duration
	resume    *DurableCheckpoint
	base      durable.BudgetCounters
	leaseErr  error
	stopLease chan struct{}
	leaseDone chan struct{}
	closeOnce sync.Once
}

// OpenStoredRun creates or resumes a run and acquires its fencing lease. A
// second worker cannot own the same run until that lease is released or expires.
func OpenStoredRun(ctx context.Context, store durable.RunStore, opts StoredRunOptions) (*StoredRun, error) {
	if store == nil {
		return nil, errors.New("agentsdk: durable run store is nil")
	}
	if opts.TenantID == "" {
		return nil, errors.New("agentsdk: durable tenant ID is required")
	}
	if opts.RunID == "" {
		opts.RunID = durable.NewRunID()
	}
	if opts.Owner == "" {
		opts.Owner = fmt.Sprintf("pid-%d", os.Getpid())
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 30 * time.Second
	}

	snapshot, _, err := store.Load(ctx, opts.TenantID, opts.RunID)
	if errors.Is(err, durable.ErrNotFound) {
		now := time.Now().UTC()
		snapshot = durable.NewRunSnapshot(opts.TenantID, opts.RunID, now)
		snapshot.Classification = opts.Classification
		snapshot.RetainUntil = opts.RetainUntil
		if err := store.Create(ctx, snapshot); err != nil {
			return nil, fmt.Errorf("agentsdk: create durable run: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("agentsdk: load durable run: %w", err)
	}
	lease, err := store.AcquireLease(ctx, opts.TenantID, opts.RunID, opts.Owner, opts.LeaseTTL)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: acquire durable run: %w", err)
	}
	s := &StoredRun{
		store: store, snapshot: snapshot, lease: lease, leaseTTL: opts.LeaseTTL,
		base: snapshot.CumulativeBudget, stopLease: make(chan struct{}), leaseDone: make(chan struct{}),
	}
	if len(snapshot.State) != 0 {
		var cp DurableCheckpoint
		if err := json.Unmarshal(snapshot.State, &cp); err != nil {
			_ = store.ReleaseLease(ctx, lease)
			return nil, fmt.Errorf("agentsdk: decode durable continuation: %w", err)
		}
		s.resume = &cp
	}
	go s.renewLeaseLoop()
	return s, nil
}

func (s *StoredRun) renewLeaseLoop() {
	defer close(s.leaseDone)
	interval := s.leaseTTL / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopLease:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			s.mu.Lock()
			if s.lease.Token == "" {
				s.mu.Unlock()
				cancel()
				return
			}
			renewed, err := s.store.RenewLease(ctx, s.lease, s.leaseTTL)
			if err == nil {
				s.lease = renewed
			}
			s.leaseErr = err
			s.mu.Unlock()
			cancel()
		}
	}
}

// ID returns the stable durable run ID.
func (s *StoredRun) ID() durable.RunID { s.mu.Lock(); defer s.mu.Unlock(); return s.snapshot.RunID }

// RunConfig returns optional Runner durability configuration. Existing callers
// may continue using RunConfig without calling this API or configuring a store.
func (s *StoredRun) RunConfig() *DurableRunConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Each Runner.Run reports usage from zero. Freeze the already committed
	// cumulative counters as this attempt's base so later attempts in the same
	// process can never reset or double-count them.
	s.base = s.snapshot.CumulativeBudget
	var resume *DurableCheckpoint
	if s.resume != nil {
		cp := *s.resume
		resume = &cp
	}
	return &DurableRunConfig{RunID: string(s.snapshot.RunID), AttemptID: string(durable.NewAttemptID()), Resume: resume, Checkpoint: s.checkpoint}
}

func (s *StoredRun) checkpoint(ctx context.Context, cp DurableCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(s.leaseErr, durable.ErrLeaseLost) {
		return fmt.Errorf("renew ownership: %w", s.leaseErr)
	}
	renewed, err := s.store.RenewLease(ctx, s.lease, s.leaseTTL)
	if err != nil {
		return fmt.Errorf("renew ownership: %w", err)
	}
	s.lease = renewed
	payload, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	next := s.snapshot
	next.Revision++
	next.State = payload
	next.Status = durable.RunRunning
	if cp.Boundary == DurableBoundaryRunCompleted {
		next.Status = durable.RunSucceeded
	} else if cp.Boundary == DurableBoundaryRunCancelled {
		next.Status = durable.RunCancelled
		next.Cancellation = &durable.Cancellation{RequestedAt: cp.CreatedAt, Reason: "runner context cancelled"}
	}
	next.UpdatedAt = cp.CreatedAt
	next.CumulativeBudget = durable.BudgetCounters{
		InputTokens:  s.base.InputTokens + cp.Usage.InputTokens,
		OutputTokens: s.base.OutputTokens + cp.Usage.OutputTokens,
		ToolCalls:    s.base.ToolCalls,
		CostMicros:   s.base.CostMicros,
		WallTimeMS:   s.base.WallTimeMS,
	}
	next.Attempts = []durable.Attempt{{ID: durable.AttemptID(cp.AttemptID), StartedAt: cp.CreatedAt}}
	next.Steps = []durable.Step{{ID: durable.StepID(cp.StepID), Kind: string(cp.Boundary), Status: "committed", StartedAt: cp.CreatedAt, EndedAt: cp.CreatedAt}}
	next.Approvals = nil
	for _, interruption := range cp.Interruptions {
		if interruption != nil {
			next.Approvals = append(next.Approvals, durable.Approval{ID: durable.ApprovalID(interruption.ToolCallID), Status: "pending", RequestedAt: cp.CreatedAt})
		}
	}
	event := durable.Event{Type: "run." + string(cp.Boundary), At: cp.CreatedAt, Classification: next.Classification, Payload: payload}
	updated, err := s.store.Append(ctx, s.lease, s.snapshot.Revision, []durable.Event{event}, next)
	if err != nil {
		return fmt.Errorf("commit checkpoint: %w", err)
	}
	s.snapshot = updated
	s.resume = &cp
	return nil
}

// Close releases this worker's fenced ownership. The persisted continuation is
// unaffected and can immediately be acquired by another process.
func (s *StoredRun) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.stopLease) })
	<-s.leaseDone
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease.Token == "" {
		return nil
	}
	err := s.store.ReleaseLease(ctx, s.lease)
	if err == nil {
		s.lease = durable.Lease{}
	}
	return err
}
