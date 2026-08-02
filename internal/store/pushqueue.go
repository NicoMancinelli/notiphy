package store

import (
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// QueuedPush is a push awaiting another delivery attempt.
type QueuedPush struct {
	ID         string
	DeviceID   string
	EventID    string
	ActivityID string
	Payload    string
	Attempts   int
	NextAt     time.Time
	LastError  string
}

// EnqueuePush queues a failed push for retry.
//
// The notification payload is stored rather than re-derived, so a retry
// delivers exactly what the first attempt would have — the event may have been
// answered or expired in the meantime, and the worker checks that separately.
func (s *Store) EnqueuePush(deviceID, eventID, activityID, payload string, firstDelay time.Duration) error {
	_, err := s.db.Exec(
		`INSERT INTO push_queue (id, device_id, event_id, activity_id, payload, attempts, next_at, done, created_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, 0, ?)`,
		model.NewID("pq"), deviceID, eventID, activityID, payload,
		time.Now().Add(firstDelay).Unix(), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("enqueue push: %w", err)
	}
	return nil
}

// DuePushes returns queued pushes ready for another attempt.
func (s *Store) DuePushes(limit int) ([]*QueuedPush, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT id, device_id, event_id, activity_id, payload, attempts, next_at, last_error
		 FROM push_queue WHERE done = 0 AND next_at <= ? ORDER BY next_at ASC LIMIT ?`,
		time.Now().Unix(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list due pushes: %w", err)
	}
	defer rows.Close()

	var out []*QueuedPush
	for rows.Next() {
		var (
			p      QueuedPush
			nextAt int64
		)
		if err := rows.Scan(&p.ID, &p.DeviceID, &p.EventID, &p.ActivityID, &p.Payload,
			&p.Attempts, &nextAt, &p.LastError); err != nil {
			return nil, fmt.Errorf("scan queued push: %w", err)
		}
		p.NextAt = fromUnix(nextAt)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// CompletePush marks a queued push delivered (or no longer worth delivering).
func (s *Store) CompletePush(id string) error {
	_, err := s.db.Exec(`UPDATE push_queue SET done = 1 WHERE id = ?`, id)
	return err
}

// FailPush records a failed attempt and schedules the next one.
func (s *Store) FailPush(id string, attempts int, nextAt time.Time, errMsg string, giveUp bool) error {
	done := 0
	if giveUp {
		done = 1
	}
	_, err := s.db.Exec(
		`UPDATE push_queue SET attempts = ?, next_at = ?, last_error = ?, done = ? WHERE id = ?`,
		attempts, nextAt.Unix(), errMsg, done, id,
	)
	return err
}

// PurgeOld trims the tables that would otherwise grow without bound on a
// long-running server. Responses and activities are kept as long as their
// parent events so a delivery record never points at a vanished row.
//
// Returns the number of rows removed across all tables.
func (s *Store) PurgeOld(age time.Duration) (int, error) {
	cutoff := time.Now().Add(-age).Unix()
	total := 0

	// Order matters: children before parents, so foreign keys stay satisfied.
	statements := []struct {
		name string
		sql  string
	}{
		{"deliveries", `DELETE FROM deliveries WHERE created_at < ?`},
		{"callbacks", `DELETE FROM callbacks WHERE done = 1 AND created_at < ?`},
		{"push_queue", `DELETE FROM push_queue WHERE done = 1 AND created_at < ?`},
		// responses cascade from events, but delete them explicitly so the
		// count is accurate and the intent is visible.
		{"responses", `DELETE FROM responses WHERE created_at < ? AND status != 'pending'`},
		{"events", `DELETE FROM events WHERE created_at < ?`},
		{"activities", `DELETE FROM activities WHERE state = 'ended' AND created_at < ?`},
	}

	for _, st := range statements {
		res, err := s.db.Exec(st.sql, cutoff)
		if err != nil {
			return total, fmt.Errorf("purge %s: %w", st.name, err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}
