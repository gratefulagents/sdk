package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FilesystemOptions configures persistence-boundary transformations.
type FilesystemOptions struct {
	Redactor  Redactor
	Encryptor Encryptor
}

// FilesystemStore is a private, tenant-confined RunStore backed by atomic JSON
// documents. Advisory file locks serialize cooperating processes.
type FilesystemStore struct {
	root      string
	redactor  Redactor
	encryptor Encryptor
}

type filesystemRecord struct {
	Document Document `json:"document"`
	Lease    *Lease   `json:"lease,omitempty"`
}

type encryptedRecord struct {
	Encrypted bool   `json:"encrypted"`
	Data      []byte `json:"data"`
}

var filesystemLocks sync.Map

// NewFilesystemStore opens a filesystem store rooted at root. All generated
// files and directories are private to the current user.
func NewFilesystemStore(root string, options ...FilesystemOptions) (*FilesystemStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("durable: filesystem root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("durable: resolve filesystem root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("durable: create filesystem root: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("durable: secure filesystem root: %w", err)
	}
	for _, name := range []string{"tenants", "locks"} {
		path := filepath.Join(absolute, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("durable: create %s: %w", name, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, fmt.Errorf("durable: secure %s: %w", name, err)
		}
	}
	var opts FilesystemOptions
	if len(options) > 0 {
		opts = options[0]
	}
	return &FilesystemStore{root: absolute, redactor: opts.Redactor, encryptor: opts.Encryptor}, nil
}

func safeFilesystemID(kind, value string) error {
	if value == "" || len(value) > 255 {
		return fmt.Errorf("durable: invalid %s", kind)
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return fmt.Errorf("durable: invalid %s", kind)
		}
	}
	return nil
}

func (s *FilesystemStore) tenantDir(tenantID TenantID) (string, error) {
	if err := safeFilesystemID("tenant ID", string(tenantID)); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "tenants", string(tenantID)), nil
}

func (s *FilesystemStore) runPath(tenantID TenantID, runID RunID) (string, error) {
	dir, err := s.tenantDir(tenantID)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("durable: tenant path is not a directory")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("durable: inspect tenant directory: %w", err)
	}
	if err := safeFilesystemID("run ID", string(runID)); err != nil {
		return "", err
	}
	return filepath.Join(dir, string(runID)+".json"), nil
}

func (s *FilesystemStore) tenantLockPath(tenantID TenantID) (string, error) {
	if err := safeFilesystemID("tenant ID", string(tenantID)); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "locks", string(tenantID)+".lock"), nil
}

func (s *FilesystemStore) withTenantLock(tenantID TenantID, fn func() error) error {
	lockPath, err := s.tenantLockPath(tenantID)
	if err != nil {
		return err
	}
	return withFileLock(lockPath, fn)
}

func (s *FilesystemStore) ensureTenantDir(tenantID TenantID) (string, error) {
	dir, err := s.tenantDir(tenantID)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return "", errors.New("durable: tenant path is not a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("durable: create tenant directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("durable: secure tenant directory: %w", err)
	}
	return dir, nil
}

// Create persists the first snapshot of a run.
func (s *FilesystemStore) Create(ctx context.Context, snapshot RunSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	return s.withTenantLock(snapshot.TenantID, func() error {
		if _, err := s.ensureTenantDir(snapshot.TenantID); err != nil {
			return err
		}
		path, err := s.runPath(snapshot.TenantID, snapshot.RunID)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(path); err == nil {
			return ErrAlreadyExists
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("durable: inspect run: %w", err)
		}
		snapshot.SchemaVersion = SchemaVersion
		if snapshot.CreatedAt.IsZero() {
			snapshot.CreatedAt = time.Now().UTC()
		}
		if snapshot.UpdatedAt.IsZero() {
			snapshot.UpdatedAt = snapshot.CreatedAt
		}
		snapshot = redactSnapshot(snapshot, s.redactor)
		return s.writeRecord(ctx, path, filesystemRecord{Document: Document{SchemaVersion: SchemaVersion, Snapshot: snapshot}})
	})
}

// Load returns a detached snapshot and immutable event history.
func (s *FilesystemStore) Load(ctx context.Context, tenantID TenantID, runID RunID) (snapshot RunSnapshot, events []Event, err error) {
	if err := ctx.Err(); err != nil {
		return RunSnapshot{}, nil, err
	}
	err = s.withTenantLock(tenantID, func() error {
		path, pathErr := s.runPath(tenantID, runID)
		if pathErr != nil {
			return pathErr
		}
		record, readErr := s.readRecord(ctx, path)
		if readErr != nil {
			return readErr
		}
		snapshot, events = record.Document.Snapshot, record.Document.Events
		return nil
	})
	return snapshot, events, err
}

// Append applies a compare-and-swap update and atomically writes events and snapshot.
func (s *FilesystemStore) Append(ctx context.Context, lease Lease, expectedRevision uint64, events []Event, snapshot RunSnapshot) (RunSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return RunSnapshot{}, err
	}
	var result RunSnapshot
	err := s.withTenantLock(lease.TenantID, func() error {
		path, pathErr := s.runPath(lease.TenantID, lease.RunID)
		if pathErr != nil {
			return pathErr
		}
		record, readErr := s.readRecord(ctx, path)
		if readErr != nil {
			return readErr
		}
		if record.Lease == nil || record.Lease.Token != lease.Token || !record.Lease.ExpiresAt.After(time.Now().UTC()) {
			return ErrLeaseLost
		}
		if record.Document.Snapshot.Revision != expectedRevision {
			return ErrConflict
		}
		updated, clean, prepareErr := prepareAppend(lease.TenantID, lease.RunID, expectedRevision, record.Document.Snapshot.EventSequence, events, snapshot, s.redactor)
		if prepareErr != nil {
			return prepareErr
		}
		record.Document.SchemaVersion, record.Document.Snapshot = SchemaVersion, updated
		record.Document.Events = append(record.Document.Events, clean...)
		if writeErr := s.writeRecord(ctx, path, record); writeErr != nil {
			return writeErr
		}
		result = updated
		return nil
	})
	return result, err
}

// AcquireLease grants ownership when no other unexpired lease exists.
func (s *FilesystemStore) AcquireLease(ctx context.Context, tenantID TenantID, runID RunID, owner string, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if owner == "" || ttl <= 0 {
		return Lease{}, errors.New("durable: lease owner and positive TTL are required")
	}
	var lease Lease
	err := s.withTenantLock(tenantID, func() error {
		path, pathErr := s.runPath(tenantID, runID)
		if pathErr != nil {
			return pathErr
		}
		record, readErr := s.readRecord(ctx, path)
		if readErr != nil {
			return readErr
		}
		now := time.Now().UTC()
		if record.Lease != nil && record.Lease.ExpiresAt.After(now) {
			return ErrLeaseHeld
		}
		lease = Lease{TenantID: tenantID, RunID: runID, Owner: owner, Token: NewLeaseToken(), ExpiresAt: now.Add(ttl)}
		record.Lease = &lease
		return s.writeRecord(ctx, path, record)
	})
	return lease, err
}

// RenewLease extends a lease only when its fencing token is still current.
func (s *FilesystemStore) RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if ttl <= 0 {
		return Lease{}, errors.New("durable: positive lease TTL is required")
	}
	var renewed Lease
	err := s.withTenantLock(lease.TenantID, func() error {
		path, pathErr := s.runPath(lease.TenantID, lease.RunID)
		if pathErr != nil {
			return pathErr
		}
		record, readErr := s.readRecord(ctx, path)
		if readErr != nil {
			return readErr
		}
		now := time.Now().UTC()
		if record.Lease == nil || record.Lease.Token != lease.Token || !record.Lease.ExpiresAt.After(now) {
			return ErrLeaseLost
		}
		renewed = *record.Lease
		renewed.ExpiresAt = now.Add(ttl)
		record.Lease = &renewed
		return s.writeRecord(ctx, path, record)
	})
	return renewed, err
}

// ReleaseLease releases ownership only for the current fencing token.
func (s *FilesystemStore) ReleaseLease(ctx context.Context, lease Lease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.withTenantLock(lease.TenantID, func() error {
		path, pathErr := s.runPath(lease.TenantID, lease.RunID)
		if pathErr != nil {
			return pathErr
		}
		record, readErr := s.readRecord(ctx, path)
		if readErr != nil {
			return readErr
		}
		if record.Lease == nil || record.Lease.Token != lease.Token {
			return ErrLeaseLost
		}
		record.Lease = nil
		return s.writeRecord(ctx, path, record)
	})
}

// DeleteRun permanently deletes one tenant-confined run.
func (s *FilesystemStore) DeleteRun(ctx context.Context, tenantID TenantID, runID RunID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.withTenantLock(tenantID, func() error {
		path, pathErr := s.runPath(tenantID, runID)
		if pathErr != nil {
			return pathErr
		}
		if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("durable: delete run: %w", err)
		}
		return nil
	})
}

// DeleteTenant permanently deletes every run for tenantID.
func (s *FilesystemStore) DeleteTenant(ctx context.Context, tenantID TenantID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.withTenantLock(tenantID, func() error {
		dir, dirErr := s.tenantDir(tenantID)
		if dirErr != nil {
			return dirErr
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("durable: delete tenant: %w", err)
		}
		return nil
	})
}

// ApplyRetention deletes snapshots whose explicit retention deadline has passed.
func (s *FilesystemStore) ApplyRetention(ctx context.Context, policy RetentionPolicy) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	now := policy.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	root := filepath.Join(s.root, "tenants")
	tenants, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("durable: list tenants: %w", err)
	}
	deleted := 0
	for _, tenant := range tenants {
		if !tenant.IsDir() || safeFilesystemID("tenant ID", tenant.Name()) != nil {
			continue
		}
		tenantID := TenantID(tenant.Name())
		err := s.withTenantLock(tenantID, func() error {
			dir, dirErr := s.tenantDir(tenantID)
			if dirErr != nil {
				return dirErr
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				return readErr
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				record, readErr := s.readRecord(ctx, filepath.Join(dir, entry.Name()))
				if readErr != nil {
					return readErr
				}
				until := record.Document.Snapshot.RetainUntil
				if until != nil && !until.After(now) {
					if removeErr := os.Remove(filepath.Join(dir, entry.Name())); removeErr != nil {
						return removeErr
					}
					deleted++
				}
			}
			return nil
		})
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

func (s *FilesystemStore) readRecord(ctx context.Context, path string) (filesystemRecord, error) {
	if err := ctx.Err(); err != nil {
		return filesystemRecord{}, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return filesystemRecord{}, ErrNotFound
	}
	if err != nil {
		return filesystemRecord{}, fmt.Errorf("durable: inspect record: %w", err)
	}
	if !info.Mode().IsRegular() {
		return filesystemRecord{}, errors.New("durable: run record is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return filesystemRecord{}, fmt.Errorf("durable: read record: %w", err)
	}
	var envelope encryptedRecord
	if err := json.Unmarshal(data, &envelope); err != nil {
		return filesystemRecord{}, fmt.Errorf("durable: decode record: %w", err)
	}
	if envelope.Encrypted {
		if s.encryptor == nil {
			return filesystemRecord{}, errors.New("durable: encrypted record requires an encryptor")
		}
		data, err = s.encryptor.Decrypt(ctx, envelope.Data)
		if err != nil {
			return filesystemRecord{}, fmt.Errorf("durable: decrypt record: %w", err)
		}
	}
	var record filesystemRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return filesystemRecord{}, fmt.Errorf("durable: decode document: %w", err)
	}
	documentData, err := json.Marshal(record.Document)
	if err != nil {
		return filesystemRecord{}, fmt.Errorf("durable: encode embedded document: %w", err)
	}
	document, err := DecodeDocument(documentData)
	if err != nil {
		return filesystemRecord{}, err
	}
	record.Document = document
	return record, nil
}

func (s *FilesystemStore) writeRecord(ctx context.Context, path string, record filesystemRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record.Document.SchemaVersion = SchemaVersion
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("durable: encode record: %w", err)
	}
	if s.encryptor != nil {
		ciphertext, encryptErr := s.encryptor.Encrypt(ctx, data)
		if encryptErr != nil {
			return fmt.Errorf("durable: encrypt record: %w", encryptErr)
		}
		data, err = json.Marshal(encryptedRecord{Encrypted: true, Data: ciphertext})
		if err != nil {
			return fmt.Errorf("durable: encode encrypted record: %w", err)
		}
	}
	return atomicWritePrivate(path, data)
}

func atomicWritePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".durable-")
	if err != nil {
		return fmt.Errorf("durable: create temporary record: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("durable: write temporary record: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("durable: sync temporary record: %w", err)
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("durable: replace record: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

var _ RunStore = (*FilesystemStore)(nil)
