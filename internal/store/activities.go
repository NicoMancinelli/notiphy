package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// ErrActivityConflict is returned when a key already has a live activity and
// the caller did not pass replace:true.
var ErrActivityConflict = errors.New("an active activity already exists for this key")

// CreateActivity starts a Live Activity. When replace is true, any existing
// active activity with the same key is ended first — this is Hark's `replace`
// flag, which exists so a re-run of a deploy reuses the device slot instead of
// failing.
func (s *Store) CreateActivity(a *model.Activity, replace bool) error {
	if a.ID == "" {
		a.ID = model.NewID("act")
	}
	if a.AccountID == "" {
		a.AccountID = DefaultAccountID
	}
	if a.Style == "" {
		a.Style = "standard"
	}
	now := time.Now().UTC()
	a.State = model.ActivityActive
	a.CreatedAt = now
	a.UpdatedAt = now

	if a.Key != "" {
		existing, err := s.ActiveActivityByKey(a.Key)
		switch {
		case err == nil && !replace:
			return ErrActivityConflict
		case err == nil:
			if err := s.EndActivity(existing.ID, "", nil); err != nil {
				return fmt.Errorf("replace activity: %w", err)
			}
		case !errors.Is(err, ErrNotFound):
			return err
		}
	}

	_, err := s.db.Exec(
		`INSERT INTO activities (id, account_id, token_id, key, title, status, progress, symbol,
		                         accent_color, style, state, seq, last_notified_progress,
		                         expires_at, stale_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?, ?)`,
		a.ID, a.AccountID, a.TokenID, a.Key, a.Title, a.Status, a.Progress, a.Symbol,
		a.AccentColor, a.Style, string(a.State),
		a.ExpiresAt.Unix(), a.StaleAt.Unix(), now.Unix(), now.Unix(),
	)
	if err != nil {
		return fmt.Errorf("create activity: %w", err)
	}
	return nil
}

const activityCols = `id, account_id, token_id, key, title, status, progress, symbol,
	accent_color, style, state, seq, last_notified_progress, last_notified_status,
	expires_at, stale_at, created_at, updated_at, ended_at`

func scanActivity(sc interface{ Scan(...any) error }) (*model.Activity, error) {
	var (
		a                model.Activity
		state            string
		progress         sql.NullFloat64
		expires, stale   int64
		created, updated int64
		ended            sql.NullInt64
	)
	if err := sc.Scan(&a.ID, &a.AccountID, &a.TokenID, &a.Key, &a.Title, &a.Status, &progress,
		&a.Symbol, &a.AccentColor, &a.Style, &state, &a.Seq, &a.LastNotifiedProgress,
		&a.LastNotifiedStatus, &expires, &stale, &created, &updated, &ended); err != nil {
		return nil, err
	}
	if progress.Valid {
		p := progress.Float64
		a.Progress = &p
	}
	a.State = model.ActivityState(state)
	a.ExpiresAt = fromUnix(expires)
	a.StaleAt = fromUnix(stale)
	a.CreatedAt = fromUnix(created)
	a.UpdatedAt = fromUnix(updated)
	a.EndedAt = fromNullUnix(ended)
	return &a, nil
}

// Activity looks up one activity by ID.
func (s *Store) Activity(id string) (*model.Activity, error) {
	row := s.db.QueryRow(`SELECT `+activityCols+` FROM activities WHERE id = ?`, id)
	a, err := scanActivity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup activity: %w", err)
	}
	return a, nil
}

// ActiveActivityByKey finds the running activity for a key.
func (s *Store) ActiveActivityByKey(key string) (*model.Activity, error) {
	row := s.db.QueryRow(
		`SELECT `+activityCols+` FROM activities
		 WHERE account_id = ? AND key = ? AND state = 'active'`,
		DefaultAccountID, key,
	)
	a, err := scanActivity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup activity by key: %w", err)
	}
	return a, nil
}

// ListActivities returns recent activities, active first then newest.
func (s *Store) ListActivities(limit int) ([]*model.Activity, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT `+activityCols+` FROM activities WHERE account_id = ?
		 ORDER BY (state = 'active') DESC, created_at DESC LIMIT ?`,
		DefaultAccountID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list activities: %w", err)
	}
	defer rows.Close()

	var out []*model.Activity
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActivityPatch carries a partial update. Nil fields are left untouched, which
// is what makes PATCH a merge rather than a replace.
type ActivityPatch struct {
	Title       *string
	Status      *string
	Progress    *float64
	Symbol      *string
	AccentColor *string
	Style       *string
}

// UpdateActivity applies a merge patch and bumps the sequence number. It
// returns the updated activity, or ErrNotFound if it already ended.
func (s *Store) UpdateActivity(id string, p ActivityPatch) (*model.Activity, error) {
	a, err := s.Activity(id)
	if err != nil {
		return nil, err
	}
	if a.State != model.ActivityActive {
		return nil, ErrNotFound
	}

	if p.Title != nil {
		a.Title = *p.Title
	}
	if p.Status != nil {
		a.Status = *p.Status
	}
	if p.Progress != nil {
		a.Progress = p.Progress
	}
	if p.Symbol != nil {
		a.Symbol = *p.Symbol
	}
	if p.AccentColor != nil {
		a.AccentColor = *p.AccentColor
	}
	if p.Style != nil {
		a.Style = *p.Style
	}
	a.Seq++
	a.UpdatedAt = time.Now().UTC()

	_, err = s.db.Exec(
		`UPDATE activities SET title = ?, status = ?, progress = ?, symbol = ?,
		        accent_color = ?, style = ?, seq = ?, updated_at = ?
		 WHERE id = ? AND state = 'active'`,
		a.Title, a.Status, a.Progress, a.Symbol, a.AccentColor, a.Style, a.Seq, a.UpdatedAt.Unix(), id,
	)
	if err != nil {
		return nil, fmt.Errorf("update activity: %w", err)
	}
	return a, nil
}

// MarkActivityNotified records the state at the last push, so the milestone
// throttle knows how far things have moved since.
func (s *Store) MarkActivityNotified(id string, progress float64, status string) error {
	_, err := s.db.Exec(
		`UPDATE activities SET last_notified_progress = ?, last_notified_status = ? WHERE id = ?`,
		progress, status, id,
	)
	return err
}

// EndActivity closes an activity, optionally applying a final status/progress.
func (s *Store) EndActivity(id string, status string, progress *float64) error {
	a, err := s.Activity(id)
	if err != nil {
		return err
	}
	if a.State != model.ActivityActive {
		return ErrNotFound
	}
	if status != "" {
		a.Status = status
	}
	if progress != nil {
		a.Progress = progress
	}
	now := time.Now().UTC()
	_, err = s.db.Exec(
		`UPDATE activities SET state = ?, status = ?, progress = ?, seq = seq + 1,
		        updated_at = ?, ended_at = ?
		 WHERE id = ? AND state = 'active'`,
		string(model.ActivityEnded), a.Status, a.Progress, now.Unix(), now.Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("end activity: %w", err)
	}
	return nil
}

// ExpireDueActivities ends activities past their expiry. Returns the count.
func (s *Store) ExpireDueActivities() (int, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`UPDATE activities SET state = ?, ended_at = ?, updated_at = ?
		 WHERE state = 'active' AND expires_at <= ?`,
		string(model.ActivityEnded), now, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("expire activities: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
