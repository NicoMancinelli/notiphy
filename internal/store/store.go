// Package store is the SQLite persistence layer. It uses the pure-Go
// modernc.org/sqlite driver so the binary builds with CGO_ENABLED=0 and ships
// in a scratch container.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// DefaultAccountID is the single account created on first boot. notiphy is
// self-hosted and single-tenant by default; the account table exists so
// multi-user is an additive change rather than a migration.
const DefaultAccountID = "acc_default"

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, applies the
// schema, and ensures the default account exists.
func Open(path string) (*Store, error) {
	// WAL keeps the SSE/live-activity readers from blocking writers, and
	// busy_timeout absorbs the brief contention that still occurs.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite tolerates one writer; letting the pool open many connections just
	// converts contention into SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	s := &Store{db: db}
	if err := s.ensureAccount(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for callers that need custom queries (dashboard stats).
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) ensureAccount() error {
	_, err := s.db.Exec(
		`INSERT INTO accounts (id, name, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		DefaultAccountID, "default", time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("ensure default account: %w", err)
	}
	return nil
}

// Setting reads a persisted setting. Missing keys return "" with no error,
// which is what callers want for optional values like the VAPID keypair.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", key, err)
	}
	return v, nil
}

// SetSetting persists a setting, overwriting any existing value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("write setting %s: %w", key, err)
	}
	return nil
}

// --- small helpers shared by the query files ---

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

func nullUnix(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func fromNullUnix(n sql.NullInt64) *time.Time {
	if !n.Valid || n.Int64 == 0 {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}

func marshalMap(m map[string]string) string {
	if m == nil {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func unmarshalMap(s string) map[string]string {
	m := map[string]string{}
	if s == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}
