package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrIdempotencyConflict signals that a key was reused with a different
// payload. Hark answers 409 for this, and so do we: silently returning the old
// result would hide a real caller bug.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different payload")

// IdempotentRecord is a previously stored response for a key.
type IdempotentRecord struct {
	StatusCode int
	Response   string
	CreatedAt  time.Time
}

// HashPayload fingerprints a request body for idempotency comparison.
func HashPayload(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// LookupIdempotency finds a stored result for (tokenID, key).
//
// It returns:
//   - (record, nil)                     when an identical request was already served
//   - (nil, ErrIdempotencyConflict)     when the key was used with a different payload
//   - (nil, ErrNotFound)                when the key is new
func (s *Store) LookupIdempotency(tokenID, key, payloadHash string) (*IdempotentRecord, error) {
	var (
		storedHash string
		rec        IdempotentRecord
		created    int64
	)
	err := s.db.QueryRow(
		`SELECT payload_hash, status_code, response, created_at
		 FROM idempotency WHERE token_id = ? AND key = ?`, tokenID, key,
	).Scan(&storedHash, &rec.StatusCode, &rec.Response, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup idempotency: %w", err)
	}
	if storedHash != payloadHash {
		return nil, ErrIdempotencyConflict
	}
	rec.CreatedAt = fromUnix(created)
	return &rec, nil
}

// SaveIdempotency stores the result of a request under its key.
func (s *Store) SaveIdempotency(tokenID, key, payloadHash string, statusCode int, response string) error {
	_, err := s.db.Exec(
		`INSERT INTO idempotency (token_id, key, payload_hash, status_code, response, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token_id, key) DO NOTHING`,
		tokenID, key, payloadHash, statusCode, response, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("save idempotency: %w", err)
	}
	return nil
}

// PurgeIdempotency drops records older than age, keeping the table bounded.
func (s *Store) PurgeIdempotency(age time.Duration) (int, error) {
	res, err := s.db.Exec(
		`DELETE FROM idempotency WHERE created_at < ?`, time.Now().Add(-age).Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("purge idempotency: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
