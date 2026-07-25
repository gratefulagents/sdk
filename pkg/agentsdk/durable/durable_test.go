package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEffectRecoveryAndTransitions(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	effect := NewEffect(RunID("run_a"), EffectNonReplayable, now)
	if effect.IdempotencyKey != IdempotencyKey(RunID("run_a"), effect.ID) {
		t.Fatal("idempotency key is not stable")
	}
	if err := TransitionEffect(&effect, EffectDispatched, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := MarkInterruptedEffect(&effect, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if effect.State != EffectOutcomeUnknown {
		t.Fatalf("interrupted effect state = %q", effect.State)
	}
	decision := RecoverEffect(effect)
	if decision.Action != RecoveryOperator || decision.Automatic {
		t.Fatalf("unknown non-replayable effect decision = %+v", decision)
	}
	for _, classification := range []EffectClassification{EffectIdempotent, EffectDeduplicated} {
		effect := Effect{Classification: classification, State: EffectOutcomeUnknown, IdempotencyKey: "stable"}
		if decision := RecoverEffect(effect); decision.Action != RecoveryRetry || !decision.Automatic {
			t.Fatalf("%s decision = %+v", classification, decision)
		}
	}
	if err := TransitionEffect(&effect, EffectDispatched, now); err == nil {
		t.Fatal("terminal recovery state accepted an invalid transition")
	}
}

func TestDecodeDocumentMigratesV1(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	v1 := V1Document{SchemaVersion: 1, TenantID: "tenant_a", RunID: "run_a", Revision: 4, Cancelled: true, BudgetTokens: 123, CreatedAt: now, UpdatedAt: now, Events: []Event{{ID: "event_a", Type: "tool.completed"}}}
	body, err := json.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	document, err := DecodeDocument(body)
	if err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != SchemaVersion || document.Snapshot.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %+v", document)
	}
	if document.Snapshot.Cancellation == nil || document.Snapshot.CumulativeBudget.InputTokens != 123 {
		t.Fatalf("migration lost state: %+v", document.Snapshot)
	}
	if document.Events[0].Sequence != 1 || document.Events[0].TenantID != "tenant_a" || document.Events[0].RunID != "run_a" {
		t.Fatalf("migration did not normalize event: %+v", document.Events[0])
	}
}

func TestFilesystemTenantIsolationCASAndRetention(t *testing.T) {
	ctx := context.Background()
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := NewRunSnapshot("tenant_a", "run_shared", now)
	second := NewRunSnapshot("tenant_b", "run_shared", now)
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireLease(ctx, first.TenantID, first.RunID, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	updated := first
	updated.Revision, updated.Status, updated.UpdatedAt = 1, RunRunning, now.Add(time.Second)
	written, err := store.Append(ctx, lease, 0, []Event{{Type: "model.completed", Payload: json.RawMessage(`{"ok":true}`)}}, updated)
	if err != nil {
		t.Fatal(err)
	}
	if written.EventSequence != 1 {
		t.Fatalf("event sequence = %d", written.EventSequence)
	}
	if _, err := store.Append(ctx, lease, 0, nil, updated); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS error = %v", err)
	}
	if err := store.ReleaseLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLease(ctx, first.TenantID, first.RunID, "worker-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	stale := written
	stale.Revision = 2
	if _, err := store.Append(ctx, lease, 1, nil, stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner append error = %v", err)
	}
	loaded, events, err := store.Load(ctx, "tenant_a", "run_shared")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 || len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("loaded = %+v, events = %+v", loaded, events)
	}
	if _, _, err := store.Load(ctx, "tenant_b", "run_shared"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(ctx, "tenant_a", "../run_shared"); err == nil {
		t.Fatal("path traversal run ID was accepted")
	}

	expired := NewRunSnapshot("tenant_a", "run_expired", now)
	deadline := now.Add(-time.Second)
	expired.RetainUntil = &deadline
	if err := store.Create(ctx, expired); err != nil {
		t.Fatal(err)
	}
	kept := NewRunSnapshot("tenant_a", "run_kept", now)
	future := now.Add(time.Hour)
	kept.RetainUntil = &future
	if err := store.Create(ctx, kept); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.ApplyRetention(ctx, RetentionPolicy{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d runs, want 1", deleted)
	}
	if _, _, err := store.Load(ctx, "tenant_a", "run_expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired run load error = %v", err)
	}
	if _, _, err := store.Load(ctx, "tenant_a", "run_kept"); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemRedactionEncryptionAndPermissions(t *testing.T) {
	ctx := context.Background()
	store, err := NewFilesystemStore(t.TempDir(), FilesystemOptions{Redactor: redactFunc(func(_ DataClassification, value json.RawMessage) json.RawMessage {
		return json.RawMessage(`"[redacted]"`)
	}), Encryptor: xorEncryptor{key: 0x5a}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	snapshot := NewRunSnapshot("tenant_a", "run_a", now)
	snapshot.Classification, snapshot.State = DataSecret, json.RawMessage(`{"token":"snapshot-secret"}`)
	if err := store.Create(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireLease(ctx, "tenant_a", "run_a", "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	updated := snapshot
	updated.Revision, updated.UpdatedAt = 1, now.Add(time.Second)
	if _, err := store.Append(ctx, lease, 0, []Event{{Type: "tool.dispatched", Classification: DataSensitive, Payload: json.RawMessage(`{"token":"event-secret"}`)}}, updated); err != nil {
		t.Fatal(err)
	}
	loaded, events, err := store.Load(ctx, "tenant_a", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.State) != `"[redacted]"` || string(events[0].Payload) != `"[redacted]"` {
		t.Fatalf("redaction failed: %s / %s", loaded.State, events[0].Payload)
	}
	runFile := filepath.Join(store.root, "tenants", "tenant_a", "run_a.json")
	raw, err := os.ReadFile(runFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("snapshot-secret")) || bytes.Contains(raw, []byte("event-secret")) {
		t.Fatal("plaintext secret persisted")
	}
	info, err := os.Stat(runFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record permissions = %o", info.Mode().Perm())
	}
}

type redactFunc func(DataClassification, json.RawMessage) json.RawMessage

func (f redactFunc) Redact(classification DataClassification, value json.RawMessage) json.RawMessage {
	return f(classification, value)
}

type xorEncryptor struct{ key byte }

func (e xorEncryptor) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	return e.apply(plaintext), nil
}
func (e xorEncryptor) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	return e.apply(ciphertext), nil
}
func (e xorEncryptor) apply(value []byte) []byte {
	out := make([]byte, len(value))
	for i := range value {
		out[i] = value[i] ^ e.key
	}
	return out
}

func TestFilesystemTenantDirectoryIsPrivate(t *testing.T) {
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), NewRunSnapshot("tenant_a", "run_a", time.Now())); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.root, "tenants", "tenant_a"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("tenant permissions = %o", info.Mode().Perm())
	}
	if strings.Contains(filepath.Clean(store.root), "..") {
		t.Fatal("unexpected non-canonical root")
	}
}
