package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	// modernc.org/sqlite is a pure-Go SQLite driver (no cgo), registered as "sqlite".
	_ "modernc.org/sqlite"
)

// sqliteStore is a SQLite-backed Store.
type sqliteStore struct {
	db *sql.DB
}

// OpenSQLite opens (creating if necessary) the SQLite database at path and
// ensures the schema exists.
func OpenSQLite(ctx context.Context, path string) (Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite handles a single writer; keep the pool small.
	db.SetMaxOpenConns(1)
	s := &sqliteStore{db: db}
	if err := s.Init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS systems (
	id       TEXT PRIMARY KEY,
	hostname TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS backups (
	id           TEXT PRIMARY KEY,
	hostname     TEXT NOT NULL,
	partition    TEXT NOT NULL,
	file_name    TEXT NOT NULL,
	compression  TEXT NOT NULL,
	scheme       TEXT NOT NULL,
	size_bytes   INTEGER NOT NULL DEFAULT 0,
	status       TEXT NOT NULL,
	message      TEXT,
	created_at   TIMESTAMP NOT NULL,
	completed_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS backup_sets (
	id           TEXT PRIMARY KEY,
	system_id    TEXT NOT NULL,
	hostname     TEXT,
	-- Items are held as JSON because they are read back whole and never
	-- queried by field. The set's manifest in the repository remains the
	-- source of truth; this table is an index that can be rebuilt from it.
	items        TEXT NOT NULL,
	status       TEXT NOT NULL,
	message      TEXT,
	created_at   TIMESTAMP NOT NULL,
	completed_at TIMESTAMP
);
`

// busyTimeout is how long a write waits for another process holding the
// database lock before giving up. Repositories that keep the database on a
// shared local path can have several backup processes writing to the same file.
const busyTimeout = 10 * time.Second

func (s *sqliteStore) Init(ctx context.Context) error {
	q := fmt.Sprintf("PRAGMA busy_timeout = %d;", busyTimeout.Milliseconds())
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	return nil
}

func (s *sqliteStore) UpsertSystem(ctx context.Context, system model.SystemInfo) error {
	const q = `INSERT INTO systems (id, hostname) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET hostname = excluded.hostname`
	if _, err := s.db.ExecContext(ctx, q, system.ID, system.Hostname); err != nil {
		return fmt.Errorf("upsert system: %w", err)
	}
	return nil
}

func (s *sqliteStore) InsertBackup(ctx context.Context, rec model.BackupRecord) error {
	const q = `INSERT INTO backups
		(id, hostname, partition, file_name, compression, scheme, size_bytes, status, message, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q,
		rec.ID, rec.Hostname, rec.Partition, rec.FileName, rec.Compression, rec.Scheme,
		rec.SizeBytes, rec.Status, rec.Message, rec.CreatedAt, rec.CompletedAt); err != nil {
		return fmt.Errorf("insert backup: %w", err)
	}
	return nil
}

// UpdateBackup records the outcome of a backup. It upserts rather than updating
// in place so that a backup whose initial "running" row is missing is still
// recorded in full instead of being silently dropped.
func (s *sqliteStore) UpdateBackup(ctx context.Context, rec model.BackupRecord) error {
	const q = `INSERT INTO backups
		(id, hostname, partition, file_name, compression, scheme, size_bytes, status, message, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			size_bytes   = excluded.size_bytes,
			status       = excluded.status,
			message      = excluded.message,
			completed_at = excluded.completed_at`
	if _, err := s.db.ExecContext(ctx, q,
		rec.ID, rec.Hostname, rec.Partition, rec.FileName, rec.Compression, rec.Scheme,
		rec.SizeBytes, rec.Status, rec.Message, rec.CreatedAt, rec.CompletedAt); err != nil {
		return fmt.Errorf("update backup: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListBackups(ctx context.Context) ([]model.BackupRecord, error) {
	const q = `SELECT id, hostname, partition, file_name, compression, scheme,
		size_bytes, status, message, created_at, completed_at
		FROM backups ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	defer rows.Close()

	var out []model.BackupRecord
	for rows.Next() {
		rec, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetBackup(ctx context.Context, id string) (model.BackupRecord, error) {
	const q = `SELECT id, hostname, partition, file_name, compression, scheme,
		size_bytes, status, message, created_at, completed_at
		FROM backups WHERE id = ?`
	return scanBackup(s.db.QueryRowContext(ctx, q, id))
}

func (s *sqliteStore) UpsertBackupSet(ctx context.Context, set model.BackupSet) error {
	items, err := json.Marshal(set.Items)
	if err != nil {
		return fmt.Errorf("encode backup set items: %w", err)
	}
	const q = `INSERT INTO backup_sets
		(id, system_id, hostname, items, status, message, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			items = excluded.items,
			status = excluded.status,
			message = excluded.message,
			completed_at = excluded.completed_at`
	if _, err := s.db.ExecContext(ctx, q, set.ID, set.SystemID, set.Hostname,
		string(items), string(set.Status), set.Message, set.CreatedAt, set.CompletedAt); err != nil {
		return fmt.Errorf("upsert backup set: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListBackupSets(ctx context.Context) ([]model.BackupSet, error) {
	const q = `SELECT id, system_id, hostname, items, status, message, created_at, completed_at
		FROM backup_sets ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list backup sets: %w", err)
	}
	defer rows.Close()

	var out []model.BackupSet
	for rows.Next() {
		var (
			set       model.BackupSet
			items     string
			hostname  sql.NullString
			message   sql.NullString
			completed sql.NullTime
			status    string
		)
		if err := rows.Scan(&set.ID, &set.SystemID, &hostname, &items,
			&status, &message, &set.CreatedAt, &completed); err != nil {
			return nil, fmt.Errorf("scan backup set: %w", err)
		}
		if err := json.Unmarshal([]byte(items), &set.Items); err != nil {
			return nil, fmt.Errorf("decode backup set items: %w", err)
		}
		set.Hostname = hostname.String
		set.Status = model.BackupStatus(status)
		set.Message = message.String
		if completed.Valid {
			t := completed.Time
			set.CompletedAt = &t
		}
		out = append(out, set)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// scanner abstracts *sql.Row and *sql.Rows for shared scanning.
type scanner interface {
	Scan(dest ...any) error
}

func scanBackup(sc scanner) (model.BackupRecord, error) {
	var (
		rec         model.BackupRecord
		message     sql.NullString
		completedAt sql.NullTime
	)
	if err := sc.Scan(
		&rec.ID, &rec.Hostname, &rec.Partition, &rec.FileName, &rec.Compression, &rec.Scheme,
		&rec.SizeBytes, &rec.Status, &message, &rec.CreatedAt, &completedAt,
	); err != nil {
		return model.BackupRecord{}, fmt.Errorf("scan backup: %w", err)
	}
	if message.Valid {
		rec.Message = message.String
	}
	if completedAt.Valid {
		t := completedAt.Time.UTC()
		rec.CompletedAt = &t
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	return rec, nil
}
