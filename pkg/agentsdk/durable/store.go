package durable

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotFound reports an absent tenant or run.
	ErrNotFound = errors.New("durable: run not found")
	// ErrAlreadyExists reports an attempt to create an existing run.
	ErrAlreadyExists = errors.New("durable: run already exists")
	// ErrConflict reports a compare-and-swap version mismatch.
	ErrConflict = errors.New("durable: revision conflict")
	// ErrLeaseHeld reports a lease owned by another unexpired worker.
	ErrLeaseHeld = errors.New("durable: lease held")
	// ErrLeaseLost reports an expired or fenced lease token.
	ErrLeaseLost = errors.New("durable: lease lost")
)

// RetentionPolicy controls expiration of runs. Runs without RetainUntil are retained.
type RetentionPolicy struct{ Now time.Time }

// RunStore is a cross-process durable execution store. Append atomically writes
// immutable events and their resulting compact snapshot when expected revision matches.
type RunStore interface {
	Create(context.Context, RunSnapshot) error
	Load(context.Context, TenantID, RunID) (RunSnapshot, []Event, error)
	Append(context.Context, Lease, uint64, []Event, RunSnapshot) (RunSnapshot, error)
	AcquireLease(context.Context, TenantID, RunID, string, time.Duration) (Lease, error)
	RenewLease(context.Context, Lease, time.Duration) (Lease, error)
	ReleaseLease(context.Context, Lease) error
	DeleteRun(context.Context, TenantID, RunID) error
	DeleteTenant(context.Context, TenantID) error
	ApplyRetention(context.Context, RetentionPolicy) (int, error)
}

func validateSnapshot(snapshot RunSnapshot) error {
	if snapshot.TenantID == "" || snapshot.RunID == "" {
		return errors.New("durable: snapshot requires tenant and run IDs")
	}
	if snapshot.SchemaVersion != 0 && snapshot.SchemaVersion != SchemaVersion {
		return fmt.Errorf("durable: snapshot schema version %d", snapshot.SchemaVersion)
	}
	return nil
}

func prepareAppend(tenantID TenantID, runID RunID, revision, sequence uint64, events []Event, snapshot RunSnapshot, redactor Redactor) (RunSnapshot, []Event, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return RunSnapshot{}, nil, err
	}
	if snapshot.TenantID != tenantID || snapshot.RunID != runID {
		return RunSnapshot{}, nil, errors.New("durable: snapshot key does not match append key")
	}
	if snapshot.Revision != revision+1 {
		return RunSnapshot{}, nil, fmt.Errorf("durable: snapshot revision must be %d", revision+1)
	}
	clean := make([]Event, len(events))
	for i, event := range events {
		if event.ID == "" {
			event.ID = NewEventID()
		}
		if event.At.IsZero() {
			event.At = snapshot.UpdatedAt.UTC()
		}
		if event.TenantID != "" && event.TenantID != tenantID || event.RunID != "" && event.RunID != runID {
			return RunSnapshot{}, nil, errors.New("durable: event key does not match append key")
		}
		event.TenantID, event.RunID, event.Sequence = tenantID, runID, sequence+uint64(i)+1
		if redactor != nil && len(event.Payload) != 0 {
			event.Payload = redactor.Redact(event.Classification, event.Payload)
		}
		clean[i] = event
	}
	snapshot.SchemaVersion = SchemaVersion
	snapshot.EventSequence = sequence + uint64(len(clean))
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = time.Now().UTC()
	}
	return redactSnapshot(snapshot, redactor), clean, nil
}

func redactSnapshot(snapshot RunSnapshot, redactor Redactor) RunSnapshot {
	if redactor == nil {
		return snapshot
	}
	if len(snapshot.State) != 0 {
		snapshot.State = redactor.Redact(snapshot.Classification, snapshot.State)
	}
	for i := range snapshot.Steps {
		if len(snapshot.Steps[i].Data) != 0 {
			snapshot.Steps[i].Data = redactor.Redact(snapshot.Classification, snapshot.Steps[i].Data)
		}
	}
	for i := range snapshot.ToolCalls {
		classification := snapshot.ToolCalls[i].Classification
		if classification == "" {
			classification = snapshot.Classification
		}
		if len(snapshot.ToolCalls[i].Input) != 0 {
			snapshot.ToolCalls[i].Input = redactor.Redact(classification, snapshot.ToolCalls[i].Input)
		}
		if len(snapshot.ToolCalls[i].Output) != 0 {
			snapshot.ToolCalls[i].Output = redactor.Redact(classification, snapshot.ToolCalls[i].Output)
		}
	}
	for i := range snapshot.Effects {
		classification := snapshot.Effects[i].DataClassification
		if classification == "" {
			classification = snapshot.Classification
		}
		if len(snapshot.Effects[i].Outcome) != 0 {
			snapshot.Effects[i].Outcome = redactor.Redact(classification, snapshot.Effects[i].Outcome)
		}
	}
	return snapshot
}
