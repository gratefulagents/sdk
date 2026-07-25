package durable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PostgresOptions configures persistence-boundary transformations for PostgresStore.
type PostgresOptions struct {
	Redactor  Redactor
	Encryptor Encryptor
}

// PostgresStore is a RunStore backed by caller-owned PostgreSQL connections.
// Call Init before using a newly provisioned database.
type PostgresStore struct {
	db        *sql.DB
	redactor  Redactor
	encryptor Encryptor
}

// NewPostgresStore constructs a PostgreSQL store using db; it never closes the
// caller-provided handle. Options are optional to keep unprotected setup concise.
func NewPostgresStore(db *sql.DB, options ...PostgresOptions) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("durable: PostgreSQL DB is required")
	}
	var opts PostgresOptions
	if len(options) > 0 {
		opts = options[0]
	}
	return &PostgresStore{db: db, redactor: opts.Redactor, encryptor: opts.Encryptor}, nil
}

// NewPostgreSQLStore is the spelling-equivalent constructor for PostgresStore.
func NewPostgreSQLStore(db *sql.DB, options ...PostgresOptions) (*PostgresStore, error) {
	return NewPostgresStore(db, options...)
}

// Init creates the tables and indexes required by PostgresStore.
func (s *PostgresStore) Init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS durable_runs (
			tenant_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			revision BIGINT NOT NULL,
			event_sequence BIGINT NOT NULL,
			snapshot BYTEA NOT NULL,
			retain_until TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			lease_owner TEXT NULL,
			lease_token TEXT NULL,
			lease_until TIMESTAMPTZ NULL,
			PRIMARY KEY (tenant_id, run_id)
		)`,
		`CREATE TABLE IF NOT EXISTS durable_events (
			tenant_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			sequence BIGINT NOT NULL,
			body BYTEA NOT NULL,
			PRIMARY KEY (tenant_id, run_id, sequence),
			FOREIGN KEY (tenant_id, run_id) REFERENCES durable_runs (tenant_id, run_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS durable_runs_retention_idx ON durable_runs (retain_until) WHERE retain_until IS NOT NULL`,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durable: begin schema init: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("durable: initialize schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durable: commit schema init: %w", err)
	}
	return nil
}

// Create persists the first snapshot of a run.
func (s *PostgresStore) Create(ctx context.Context, snapshot RunSnapshot) error {
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = snapshot.CreatedAt
	}
	snapshot.SchemaVersion = SchemaVersion
	snapshot = redactSnapshot(snapshot, s.redactor)
	body, err := s.encode(ctx, snapshot)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO durable_runs
		(tenant_id, run_id, revision, event_sequence, snapshot, retain_until, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, run_id) DO NOTHING`,
		snapshot.TenantID, snapshot.RunID, snapshot.Revision, snapshot.EventSequence, body, snapshot.RetainUntil, snapshot.CreatedAt, snapshot.UpdatedAt)
	if err != nil {
		return fmt.Errorf("durable: create run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("durable: inspect create: %w", err)
	}
	if affected != 1 {
		return ErrAlreadyExists
	}
	return nil
}

// Load returns a detached compact snapshot and its immutable event stream.
func (s *PostgresStore) Load(ctx context.Context, tenantID TenantID, runID RunID) (RunSnapshot, []Event, error) {
	var body []byte
	if err := s.db.QueryRowContext(ctx, `SELECT snapshot FROM durable_runs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunSnapshot{}, nil, ErrNotFound
		}
		return RunSnapshot{}, nil, fmt.Errorf("durable: load run: %w", err)
	}
	var snapshot RunSnapshot
	if err := s.decode(ctx, body, &snapshot); err != nil {
		return RunSnapshot{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM durable_events WHERE tenant_id = $1 AND run_id = $2 AND sequence <= $3 ORDER BY sequence`, tenantID, runID, snapshot.EventSequence)
	if err != nil {
		return RunSnapshot{}, nil, fmt.Errorf("durable: load events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var eventBody []byte
		if err := rows.Scan(&eventBody); err != nil {
			return RunSnapshot{}, nil, fmt.Errorf("durable: scan event: %w", err)
		}
		var event Event
		if err := s.decode(ctx, eventBody, &event); err != nil {
			return RunSnapshot{}, nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return RunSnapshot{}, nil, fmt.Errorf("durable: read events: %w", err)
	}
	return snapshot, events, nil
}

// Append atomically locks the run row, compares revision, inserts immutable
// events, and writes the resulting compact snapshot.
func (s *PostgresStore) Append(ctx context.Context, lease Lease, expectedRevision uint64, events []Event, snapshot RunSnapshot) (RunSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("durable: begin append: %w", err)
	}
	defer tx.Rollback()
	var revision, sequence uint64
	var leaseToken sql.NullString
	var leaseUntil sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT revision, event_sequence, lease_token, lease_until FROM durable_runs WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, lease.TenantID, lease.RunID).Scan(&revision, &sequence, &leaseToken, &leaseUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunSnapshot{}, ErrNotFound
		}
		return RunSnapshot{}, fmt.Errorf("durable: lock run: %w", err)
	}
	if !leaseToken.Valid || LeaseToken(leaseToken.String) != lease.Token || !leaseUntil.Valid || !leaseUntil.Time.After(time.Now().UTC()) {
		return RunSnapshot{}, ErrLeaseLost
	}
	if revision != expectedRevision {
		return RunSnapshot{}, ErrConflict
	}
	updated, clean, err := prepareAppend(lease.TenantID, lease.RunID, revision, sequence, events, snapshot, s.redactor)
	if err != nil {
		return RunSnapshot{}, err
	}
	for _, event := range clean {
		body, encodeErr := s.encode(ctx, event)
		if encodeErr != nil {
			return RunSnapshot{}, encodeErr
		}
		if _, insertErr := tx.ExecContext(ctx, `INSERT INTO durable_events (tenant_id, run_id, sequence, body) VALUES ($1, $2, $3, $4)`, lease.TenantID, lease.RunID, event.Sequence, body); insertErr != nil {
			return RunSnapshot{}, fmt.Errorf("durable: append event: %w", insertErr)
		}
	}
	body, err := s.encode(ctx, updated)
	if err != nil {
		return RunSnapshot{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE durable_runs SET revision = $1, event_sequence = $2, snapshot = $3, retain_until = $4, updated_at = $5 WHERE tenant_id = $6 AND run_id = $7 AND revision = $8`, updated.Revision, updated.EventSequence, body, updated.RetainUntil, updated.UpdatedAt, lease.TenantID, lease.RunID, revision)
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("durable: update snapshot: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("durable: inspect append: %w", err)
	}
	if affected != 1 {
		return RunSnapshot{}, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return RunSnapshot{}, fmt.Errorf("durable: commit append: %w", err)
	}
	return updated, nil
}

// AcquireLease grants exclusive ownership using a row-level lock.
func (s *PostgresStore) AcquireLease(ctx context.Context, tenantID TenantID, runID RunID, owner string, ttl time.Duration) (Lease, error) {
	if owner == "" || ttl <= 0 {
		return Lease{}, errors.New("durable: lease owner and positive TTL are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("durable: begin lease: %w", err)
	}
	defer tx.Rollback()
	var token, currentOwner sql.NullString
	var until sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner, lease_token, lease_until FROM durable_runs WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, runID).Scan(&currentOwner, &token, &until); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Lease{}, ErrNotFound
		}
		return Lease{}, fmt.Errorf("durable: lock lease: %w", err)
	}
	now := time.Now().UTC()
	if until.Valid && until.Time.After(now) {
		return Lease{}, ErrLeaseHeld
	}
	lease := Lease{TenantID: tenantID, RunID: runID, Owner: owner, Token: NewLeaseToken(), ExpiresAt: now.Add(ttl)}
	if _, err := tx.ExecContext(ctx, `UPDATE durable_runs SET lease_owner = $1, lease_token = $2, lease_until = $3 WHERE tenant_id = $4 AND run_id = $5`, lease.Owner, lease.Token, lease.ExpiresAt, tenantID, runID); err != nil {
		return Lease{}, fmt.Errorf("durable: write lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("durable: commit lease: %w", err)
	}
	return lease, nil
}

// RenewLease extends the currently fenced lease.
func (s *PostgresStore) RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if ttl <= 0 {
		return Lease{}, errors.New("durable: positive lease TTL is required")
	}
	now := time.Now().UTC()
	renewed := lease
	renewed.ExpiresAt = now.Add(ttl)
	result, err := s.db.ExecContext(ctx, `UPDATE durable_runs SET lease_until = $1 WHERE tenant_id = $2 AND run_id = $3 AND lease_token = $4 AND lease_until > $5`, renewed.ExpiresAt, lease.TenantID, lease.RunID, lease.Token, now)
	if err != nil {
		return Lease{}, fmt.Errorf("durable: renew lease: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Lease{}, err
	}
	if affected != 1 {
		return Lease{}, ErrLeaseLost
	}
	return renewed, nil
}

// ReleaseLease releases a lease only for its current fencing token.
func (s *PostgresStore) ReleaseLease(ctx context.Context, lease Lease) error {
	result, err := s.db.ExecContext(ctx, `UPDATE durable_runs SET lease_owner = NULL, lease_token = NULL, lease_until = NULL WHERE tenant_id = $1 AND run_id = $2 AND lease_token = $3`, lease.TenantID, lease.RunID, lease.Token)
	if err != nil {
		return fmt.Errorf("durable: release lease: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLeaseLost
	}
	return nil
}

// DeleteRun permanently deletes one run and cascades its immutable events.
func (s *PostgresStore) DeleteRun(ctx context.Context, tenantID TenantID, runID RunID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID)
	if err != nil {
		return fmt.Errorf("durable: delete run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrNotFound
	}
	return nil
}

// DeleteTenant permanently deletes all runs belonging to tenantID.
func (s *PostgresStore) DeleteTenant(ctx context.Context, tenantID TenantID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return fmt.Errorf("durable: delete tenant: %w", err)
	}
	return nil
}

// ApplyRetention permanently removes runs whose explicit deadline has elapsed.
func (s *PostgresStore) ApplyRetention(ctx context.Context, policy RetentionPolicy) (int, error) {
	now := policy.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM durable_runs WHERE retain_until IS NOT NULL AND retain_until <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("durable: apply retention: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

func (s *PostgresStore) encode(ctx context.Context, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("durable: encode value: %w", err)
	}
	if s.encryptor == nil {
		return body, nil
	}
	ciphertext, err := s.encryptor.Encrypt(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("durable: encrypt value: %w", err)
	}
	body, err = json.Marshal(encryptedRecord{Encrypted: true, Data: ciphertext})
	if err != nil {
		return nil, fmt.Errorf("durable: encode encrypted value: %w", err)
	}
	return body, nil
}

func (s *PostgresStore) decode(ctx context.Context, body []byte, value any) error {
	var envelope encryptedRecord
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("durable: decode value: %w", err)
	}
	if envelope.Encrypted {
		if s.encryptor == nil {
			return errors.New("durable: encrypted value requires an encryptor")
		}
		var err error
		body, err = s.encryptor.Decrypt(ctx, envelope.Data)
		if err != nil {
			return fmt.Errorf("durable: decrypt value: %w", err)
		}
	}
	if err := json.Unmarshal(body, value); err != nil {
		return fmt.Errorf("durable: decode value: %w", err)
	}
	return nil
}

var _ RunStore = (*PostgresStore)(nil)
