package projectstate

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// busyTimeoutMS bounds how long a writer waits for the WAL write lock before
// returning SQLITE_BUSY. The engine serializes in-process writers with a mutex,
// so this only matters for separate processes sharing one database file.
const sqliteBusyTimeoutMS = 5000

// SQLiteOptions configures a SQLiteStore. Exactly one of DB or Path supplies the
// database: pass DB to share an already-open handle (for example the assistant's
// shared state.db) or Path to have the store open and own its own file.
type SQLiteOptions struct {
	// DB is an existing database handle to use. When set, the store does not
	// close it and assumes the caller has configured WAL/pragmas. Table names
	// are namespaced (see TablePrefix) so projectstate can coexist with other
	// tables in a shared database.
	DB *sql.DB
	// Path opens (creating if necessary) a dedicated database file. Ignored when
	// DB is set.
	Path string
	// TablePrefix namespaces the store's tables. Defaults to "projectstate_".
	TablePrefix string

	ProjectID string
	WorkDir   string
	Actor     string
	RunID     string
	// Embedder enables embeddings-backed hybrid memory recall. When nil,
	// SearchMemories falls back to lexical keyword search.
	Embedder Embedder
	// Hybrid tunes lexical/semantic fusion. When nil, DefaultHybridConfig is used.
	Hybrid *HybridConfig
}

// SQLiteStore persists durable project state in SQLite. It reuses the same
// event-sourced engine as FilesystemStore: events live in a table and the
// in-memory state is rebuilt by replaying them, so behavior is identical to the
// filesystem store while gaining transactional durability and a shareable DB.
type SQLiteStore struct {
	*engine
	backend *sqliteBackend
}

// NewSQLiteStore opens a SQLite-backed project state store.
func NewSQLiteStore(opts SQLiteOptions) (*SQLiteStore, error) {
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir != "" {
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = abs
		}
	}
	projectID := sanitizeProjectID(opts.ProjectID)
	if projectID == "" {
		projectID = DeriveProjectID(workDir)
	}
	prefix := strings.TrimSpace(opts.TablePrefix)
	if prefix == "" {
		prefix = "projectstate_"
	}

	be := &sqliteBackend{
		projectID:   projectID,
		eventsTable: prefix + "events",
		embedTable:  prefix + "embeddings",
	}
	var stateDir string
	if opts.DB != nil {
		be.db = opts.DB
	} else {
		path := strings.TrimSpace(opts.Path)
		if path == "" {
			return nil, fmt.Errorf("projectstate: SQLiteOptions requires DB or Path")
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("create db dir: %w", err)
			}
		}
		db, err := openSQLiteFile(path)
		if err != nil {
			return nil, err
		}
		_ = os.Chmod(path, 0o600)
		be.db = db
		be.ownDB = true
		stateDir = filepath.Dir(path)
	}

	if err := be.migrate(); err != nil {
		if be.ownDB {
			_ = be.db.Close()
		}
		return nil, err
	}

	hybrid := DefaultHybridConfig()
	if opts.Hybrid != nil {
		hybrid = *opts.Hybrid
	}
	eng := &engine{
		projectID: projectID,
		workDir:   workDir,
		actor:     strings.TrimSpace(opts.Actor),
		runID:     strings.TrimSpace(opts.RunID),
		stateDir:  stateDir,
		embedder:  opts.Embedder,
		hybrid:    hybrid,
		backend:   be,
	}
	store := &SQLiteStore{engine: eng, backend: be}
	if err := eng.initialize(context.Background()); err != nil {
		if be.ownDB {
			_ = be.db.Close()
		}
		return nil, err
	}
	return store, nil
}

// DB returns the underlying database handle so callers can share it.
func (s *SQLiteStore) DB() *sql.DB { return s.backend.db }

func openSQLiteFile(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?" + url.Values{
		"_txlock": []string{"immediate"},
		"_pragma": []string{
			fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMS),
			"journal_mode(WAL)",
			"synchronous(NORMAL)",
			"foreign_keys(on)",
		},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes writers, avoiding SQLITE_BUSY convoys under
	// WAL. Throughput here is tiny (one agent's durable state).
	db.SetMaxOpenConns(1)
	return db, nil
}

// sqliteBackend implements backend on top of a SQLite database. Events are
// scoped by project_id so multiple projects can share one database file.
type sqliteBackend struct {
	db          *sql.DB
	ownDB       bool
	projectID   string
	eventsTable string
	embedTable  string
}

func (b *sqliteBackend) migrate() error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			project_id TEXT NOT NULL,
			seq        INTEGER NOT NULL,
			event_id   TEXT NOT NULL,
			run_id     TEXT,
			actor      TEXT,
			ts         INTEGER NOT NULL,
			type       TEXT NOT NULL,
			payload    BLOB,
			PRIMARY KEY (project_id, seq)
		)`, b.eventsTable),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			project_id TEXT NOT NULL,
			memory_id  TEXT NOT NULL,
			hash       TEXT NOT NULL,
			model      TEXT NOT NULL,
			dims       INTEGER NOT NULL,
			vector     BLOB NOT NULL,
			PRIMARY KEY (project_id, memory_id)
		)`, b.embedTable),
	}
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("projectstate sqlite migrate: %w", err)
		}
	}
	return tx.Commit()
}

func (b *sqliteBackend) close() error {
	if b.ownDB {
		return b.db.Close()
	}
	return nil
}

// lock is a no-op: the engine serializes in-process access with its mutex and
// SQLite's own locking (busy_timeout + the single write connection) serializes
// across processes.
func (b *sqliteBackend) lock(_ context.Context) (func(), error) {
	return func() {}, nil
}

func (b *sqliteBackend) loadEvents() ([]Event, error) {
	rows, err := b.db.Query(
		fmt.Sprintf(`SELECT seq, event_id, run_id, actor, ts, type, payload FROM %s WHERE project_id = ? ORDER BY seq`, b.eventsTable),
		b.projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var (
			ev      Event
			runID   sql.NullString
			actor   sql.NullString
			ts      int64
			payload []byte
		)
		if err := rows.Scan(&ev.Seq, &ev.EventID, &runID, &actor, &ts, &ev.Type, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		ev.ProjectID = b.projectID
		ev.RunID = runID.String
		ev.Actor = actor.String
		ev.Time = time.Unix(0, ts).UTC()
		if len(payload) > 0 {
			ev.Payload = append([]byte(nil), payload...)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

func (b *sqliteBackend) appendEvent(ev Event) error {
	_, err := b.db.Exec(
		fmt.Sprintf(`INSERT INTO %s (project_id, seq, event_id, run_id, actor, ts, type, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, b.eventsTable),
		b.projectID, ev.Seq, ev.EventID, nullString(ev.RunID), nullString(ev.Actor), ev.Time.UTC().UnixNano(), ev.Type, []byte(ev.Payload),
	)
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// snapshot is a no-op: state is always rebuilt from the events table.
func (b *sqliteBackend) snapshot(_ *state) error { return nil }

func (b *sqliteBackend) readEmbeddings() (map[string]embeddingRecord, error) {
	rows, err := b.db.Query(
		fmt.Sprintf(`SELECT memory_id, hash, model, dims, vector FROM %s WHERE project_id = ?`, b.embedTable),
		b.projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("read embeddings: %w", err)
	}
	defer rows.Close()
	out := map[string]embeddingRecord{}
	for rows.Next() {
		var (
			id     string
			rec    embeddingRecord
			vector []byte
		)
		if err := rows.Scan(&id, &rec.Hash, &rec.Model, &rec.Dims, &vector); err != nil {
			return nil, fmt.Errorf("scan embedding: %w", err)
		}
		rec.Vector = decodeVector(vector)
		out[id] = rec
	}
	return out, rows.Err()
}

func (b *sqliteBackend) writeEmbeddings(records map[string]embeddingRecord, _ string) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE project_id = ?`, b.embedTable), b.projectID); err != nil {
		return fmt.Errorf("clear embeddings: %w", err)
	}
	stmt, err := tx.Prepare(fmt.Sprintf(`INSERT INTO %s (project_id, memory_id, hash, model, dims, vector) VALUES (?, ?, ?, ?, ?, ?)`, b.embedTable))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for id, rec := range records {
		if len(rec.Vector) == 0 {
			continue
		}
		if _, err := stmt.Exec(b.projectID, id, rec.Hash, rec.Model, rec.Dims, encodeVector(rec.Vector)); err != nil {
			return fmt.Errorf("insert embedding: %w", err)
		}
	}
	return tx.Commit()
}

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// encodeVector packs a float32 slice into a little-endian byte blob.
func encodeVector(vec []float32) []byte {
	out := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out
}

// decodeVector unpacks a little-endian byte blob into a float32 slice.
func decodeVector(data []byte) []float32 {
	n := len(data) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return out
}
