package store

import (
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// PendingCallback is a queued outbound callback delivery.
type PendingCallback struct {
	ID         string
	ResponseID string
	URL        string
	Token      string
	Payload    string
	Attempts   int
	NextAt     time.Time
	Done       bool
	LastError  string
}

// EnqueueCallback queues a callback for immediate first delivery.
func (s *Store) EnqueueCallback(responseID, url, token, payload string) error {
	now := time.Now()
	_, err := s.db.Exec(
		`INSERT INTO callbacks (id, response_id, url, token, payload, attempts, next_at, done, created_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?, 0, ?)`,
		model.NewID("cb"), responseID, url, token, payload, now.Unix(), now.Unix(),
	)
	if err != nil {
		return fmt.Errorf("enqueue callback: %w", err)
	}
	return nil
}

// DueCallbacks returns callbacks ready for another attempt.
func (s *Store) DueCallbacks(limit int) ([]*PendingCallback, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT id, response_id, url, token, payload, attempts, next_at, last_error
		 FROM callbacks WHERE done = 0 AND next_at <= ? ORDER BY next_at ASC LIMIT ?`,
		time.Now().Unix(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list due callbacks: %w", err)
	}
	defer rows.Close()

	var out []*PendingCallback
	for rows.Next() {
		var (
			c      PendingCallback
			nextAt int64
		)
		if err := rows.Scan(&c.ID, &c.ResponseID, &c.URL, &c.Token, &c.Payload, &c.Attempts, &nextAt, &c.LastError); err != nil {
			return nil, fmt.Errorf("scan callback: %w", err)
		}
		c.NextAt = fromUnix(nextAt)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// CompleteCallback marks a callback delivered.
func (s *Store) CompleteCallback(id string) error {
	_, err := s.db.Exec(`UPDATE callbacks SET done = 1, attempts = attempts + 1 WHERE id = ?`, id)
	return err
}

// FailCallback records a failed attempt and schedules the next one. After the
// final attempt the callback is marked done so it stops consuming the queue.
func (s *Store) FailCallback(id string, attempts int, nextAt time.Time, errMsg string, giveUp bool) error {
	done := 0
	if giveUp {
		done = 1
	}
	_, err := s.db.Exec(
		`UPDATE callbacks SET attempts = ?, next_at = ?, last_error = ?, done = ? WHERE id = ?`,
		attempts, nextAt.Unix(), errMsg, done, id,
	)
	return err
}
