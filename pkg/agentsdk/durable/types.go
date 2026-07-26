package durable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the current durable JSON document format.
const SchemaVersion = 2

// Typed identifiers prevent accidentally mixing durable resource keys.
type (
	TenantID   string
	RunID      string
	AttemptID  string
	StepID     string
	ToolCallID string
	ApprovalID string
	ChildRunID string
	EffectID   string
	EventID    string
	LeaseToken string
)

func newID(prefix string) string { return prefix + "_" + uuid.NewString() }

// NewTenantID creates a stable tenant identifier suitable for a durable store.
func NewTenantID() TenantID { return TenantID(newID("ten")) }

// NewRunID creates a new run identifier.
func NewRunID() RunID { return RunID(newID("run")) }

// NewAttemptID creates a new attempt identifier.
func NewAttemptID() AttemptID { return AttemptID(newID("att")) }

// NewStepID creates a new step identifier.
func NewStepID() StepID { return StepID(newID("step")) }

// NewToolCallID creates a new tool-call identifier.
func NewToolCallID() ToolCallID { return ToolCallID(newID("tool")) }

// NewApprovalID creates a new approval identifier.
func NewApprovalID() ApprovalID { return ApprovalID(newID("approval")) }

// NewChildRunID creates a new child-run identifier.
func NewChildRunID() ChildRunID { return ChildRunID(newID("child")) }

// NewEffectID creates a new external-effect identifier.
func NewEffectID() EffectID { return EffectID(newID("effect")) }

// NewEventID creates a new immutable event identifier.
func NewEventID() EventID { return EventID(newID("event")) }

// DataClassification communicates how persisted data must be handled.
type DataClassification string

const (
	DataPublic    DataClassification = "public"
	DataInternal  DataClassification = "internal"
	DataSensitive DataClassification = "sensitive"
	DataSecret    DataClassification = "secret"
)

// Redactor transforms data before it crosses a persistence boundary.
type Redactor interface {
	Redact(classification DataClassification, value json.RawMessage) json.RawMessage
}

// Encryptor protects complete persisted records. Key management remains with
// the caller; stores never retain encryption keys.
type Encryptor interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// RunStatus is the durable lifecycle of a run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// Attempt records one model or execution attempt.
type Attempt struct {
	ID        AttemptID `json:"id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
}

// Step records a resumable run boundary.
type Step struct {
	ID        StepID          `json:"id"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data,omitempty"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   time.Time       `json:"ended_at,omitempty"`
}

// ToolCall records a durable tool invocation boundary.
type ToolCall struct {
	ID             ToolCallID         `json:"id"`
	Name           string             `json:"name"`
	Status         string             `json:"status"`
	Classification DataClassification `json:"classification,omitempty"`
	Input          json.RawMessage    `json:"input,omitempty"`
	Output         json.RawMessage    `json:"output,omitempty"`
	StartedAt      time.Time          `json:"started_at"`
	EndedAt        time.Time          `json:"ended_at,omitempty"`
}

// Approval records an approval request and its resolution.
type Approval struct {
	ID          ApprovalID `json:"id"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	ResolvedAt  time.Time  `json:"resolved_at,omitempty"`
	ResolvedBy  string     `json:"resolved_by,omitempty"`
}

// ChildRun records a spawned run that must be resumed or reconciled explicitly.
type ChildRun struct {
	ID        ChildRunID `json:"id"`
	RunID     RunID      `json:"run_id"`
	Status    RunStatus  `json:"status"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   time.Time  `json:"ended_at,omitempty"`
}

// Cancellation persists a cancellation request independently of process life.
type Cancellation struct {
	RequestedAt    time.Time `json:"requested_at,omitempty"`
	RequestedBy    string    `json:"requested_by,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	AcknowledgedAt time.Time `json:"acknowledged_at,omitempty"`
}

// BudgetCounters are cumulative values, never per-process deltas.
type BudgetCounters struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	ToolCalls    int64 `json:"tool_calls,omitempty"`
	CostMicros   int64 `json:"cost_micros,omitempty"`
	WallTimeMS   int64 `json:"wall_time_ms,omitempty"`
}

// EffectClassification determines whether an uncertain external effect can be replayed.
type EffectClassification string

const (
	EffectIdempotent    EffectClassification = "idempotent"
	EffectDeduplicated  EffectClassification = "deduplicated"
	EffectNonReplayable EffectClassification = "non_replayable"
)

// EffectState describes the durable external-effect protocol.
type EffectState string

const (
	EffectPrepared       EffectState = "prepared"
	EffectDispatched     EffectState = "dispatched"
	EffectSucceeded      EffectState = "succeeded"
	EffectFailed         EffectState = "failed"
	EffectOutcomeUnknown EffectState = "outcome_unknown"
)

// Effect persists intent and outcome around an external side effect.
type Effect struct {
	ID                 EffectID             `json:"id"`
	Classification     EffectClassification `json:"classification"`
	DataClassification DataClassification   `json:"data_classification,omitempty"`
	State              EffectState          `json:"state"`
	IdempotencyKey     string               `json:"idempotency_key,omitempty"`
	PreparedAt         time.Time            `json:"prepared_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
	Outcome            json.RawMessage      `json:"outcome,omitempty"`
}

// NewEffect prepares an effect with an idempotency key stable across restarts.
func NewEffect(runID RunID, classification EffectClassification, now time.Time) Effect {
	id := NewEffectID()
	return Effect{ID: id, Classification: classification, State: EffectPrepared, IdempotencyKey: IdempotencyKey(runID, id), PreparedAt: now.UTC(), UpdatedAt: now.UTC()}
}

// IdempotencyKey derives a stable opaque key for a run-effect pair.
func IdempotencyKey(runID RunID, effectID EffectID) string {
	sum := sha256.Sum256([]byte(string(runID) + ":" + string(effectID)))
	return "ga_" + hex.EncodeToString(sum[:])
}

// RecoveryAction tells a resumer what it may safely do with an interrupted effect.
type RecoveryAction string

const (
	RecoveryNone      RecoveryAction = "none"
	RecoveryRetry     RecoveryAction = "retry"
	RecoveryReconcile RecoveryAction = "reconcile"
	RecoveryOperator  RecoveryAction = "operator_resolution"
)

// RecoveryDecision is deliberately conservative: unknown non-replayable
// effects require an operator and are never automatically retried.
type RecoveryDecision struct {
	Action         RecoveryAction `json:"action"`
	Automatic      bool           `json:"automatic"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
}

// MarkInterruptedEffect converts an ambiguous dispatched effect to the
// explicit outcome-unknown state before recovery policy is evaluated.
func MarkInterruptedEffect(effect *Effect, now time.Time) error {
	if effect == nil {
		return errors.New("durable: nil effect")
	}
	if effect.State != EffectDispatched {
		return nil
	}
	return TransitionEffect(effect, EffectOutcomeUnknown, now)
}

// RecoverEffect returns the only safe recovery action for an effect.
func RecoverEffect(effect Effect) RecoveryDecision {
	decision := RecoveryDecision{Action: RecoveryNone, IdempotencyKey: effect.IdempotencyKey}
	switch effect.State {
	case EffectPrepared:
		decision.Action, decision.Automatic = RecoveryRetry, true
	case EffectDispatched:
		if effect.Classification == EffectNonReplayable {
			decision.Action = RecoveryReconcile
		} else {
			decision.Action, decision.Automatic = RecoveryRetry, true
		}
	case EffectOutcomeUnknown:
		if effect.Classification == EffectNonReplayable {
			decision.Action = RecoveryOperator
		} else {
			decision.Action, decision.Automatic = RecoveryRetry, true
		}
	}
	return decision
}

// TransitionEffect validates a durable external-effect state transition.
func TransitionEffect(effect *Effect, next EffectState, now time.Time) error {
	if effect == nil {
		return errors.New("durable: nil effect")
	}
	valid := (effect.State == EffectPrepared && (next == EffectDispatched || next == EffectFailed)) ||
		(effect.State == EffectDispatched && (next == EffectSucceeded || next == EffectFailed || next == EffectOutcomeUnknown)) ||
		(effect.State == EffectOutcomeUnknown && (next == EffectSucceeded || next == EffectFailed))
	if !valid {
		return fmt.Errorf("durable: invalid effect transition %q -> %q", effect.State, next)
	}
	effect.State, effect.UpdatedAt = next, now.UTC()
	return nil
}

// Event is immutable once appended. Sequence is assigned by the store.
type Event struct {
	ID             EventID            `json:"id"`
	TenantID       TenantID           `json:"tenant_id"`
	RunID          RunID              `json:"run_id"`
	Sequence       uint64             `json:"sequence"`
	At             time.Time          `json:"at"`
	Type           string             `json:"type"`
	Classification DataClassification `json:"classification,omitempty"`
	Payload        json.RawMessage    `json:"payload,omitempty"`
}

// RunSnapshot is a compact continuation point. State holds runner-specific
// continuation data while the typed fields expose durable scheduling boundaries.
type RunSnapshot struct {
	SchemaVersion    int                `json:"schema_version"`
	TenantID         TenantID           `json:"tenant_id"`
	RunID            RunID              `json:"run_id"`
	Revision         uint64             `json:"revision"`
	EventSequence    uint64             `json:"event_sequence"`
	Status           RunStatus          `json:"status"`
	Classification   DataClassification `json:"classification,omitempty"`
	State            json.RawMessage    `json:"state,omitempty"`
	Attempts         []Attempt          `json:"attempts,omitempty"`
	Steps            []Step             `json:"steps,omitempty"`
	ToolCalls        []ToolCall         `json:"tool_calls,omitempty"`
	Approvals        []Approval         `json:"approvals,omitempty"`
	ChildRuns        []ChildRun         `json:"child_runs,omitempty"`
	Effects          []Effect           `json:"effects,omitempty"`
	Cancellation     *Cancellation      `json:"cancellation,omitempty"`
	CumulativeBudget BudgetCounters     `json:"cumulative_budget"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
	RetainUntil      *time.Time         `json:"retain_until,omitempty"`
}

// NewRunSnapshot creates the first compact continuation for a run.
func NewRunSnapshot(tenantID TenantID, runID RunID, now time.Time) RunSnapshot {
	return RunSnapshot{SchemaVersion: SchemaVersion, TenantID: tenantID, RunID: runID, Status: RunPending, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
}

// Lease grants one worker temporary ownership of a run.
type Lease struct {
	TenantID  TenantID   `json:"tenant_id"`
	RunID     RunID      `json:"run_id"`
	Owner     string     `json:"owner"`
	Token     LeaseToken `json:"token"`
	ExpiresAt time.Time  `json:"expires_at"`
}

// NewLeaseToken creates an opaque lease fencing token.
func NewLeaseToken() LeaseToken { return LeaseToken(newID("lease")) }
