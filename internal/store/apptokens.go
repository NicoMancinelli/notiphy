package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// AppToken authorizes an installed PWA.
//
// It is a capability, like the webhook and approval tokens: holding it grants
// access to the app shell and its own state, and nothing else. It is
// deliberately *not* the admin token, so a phone that gets handed one cannot
// mint webhook credentials or register other devices.
type AppToken struct {
	ID        string
	Token     string
	Label     string
	DeviceID  string
	CreatedAt time.Time
	LastSeen  *time.Time
}

// CreateAppToken mints a token for a PWA install.
func (s *Store) CreateAppToken(label string) (*AppToken, error) {
	t := &AppToken{
		ID:        model.NewID("apt"),
		Token:     model.NewID("app"),
		Label:     label,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO app_tokens (id, token, label, device_id, created_at) VALUES (?, ?, ?, '', ?)`,
		t.ID, t.Token, t.Label, t.CreatedAt.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("create app token: %w", err)
	}
	return t, nil
}

// AppTokenByValue resolves a raw app token and records that it was used.
func (s *Store) AppTokenByValue(value string) (*AppToken, error) {
	var (
		t        AppToken
		created  int64
		lastSeen sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT id, token, label, device_id, created_at, last_seen FROM app_tokens WHERE token = ?`,
		value,
	).Scan(&t.ID, &t.Token, &t.Label, &t.DeviceID, &created, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup app token: %w", err)
	}
	t.CreatedAt = fromUnix(created)
	t.LastSeen = fromNullUnix(lastSeen)

	if _, err := s.db.Exec(`UPDATE app_tokens SET last_seen = ? WHERE id = ?`, time.Now().Unix(), t.ID); err != nil {
		return nil, fmt.Errorf("touch app token: %w", err)
	}
	return &t, nil
}

// BindAppTokenDevice associates an app token with the device it registered, so
// the shell can show that install's own state.
func (s *Store) BindAppTokenDevice(tokenID, deviceID string) error {
	_, err := s.db.Exec(`UPDATE app_tokens SET device_id = ? WHERE id = ?`, deviceID, tokenID)
	if err != nil {
		return fmt.Errorf("bind app token device: %w", err)
	}
	return nil
}

// PendingResponses returns interactive responses still awaiting an answer,
// newest first. This drives the app shell's list and its badge count.
func (s *Store) PendingResponses(limit int) ([]*model.Response, []*model.Event, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT `+responseCols+` FROM responses
		 WHERE status = ? AND expires_at > ? ORDER BY created_at DESC LIMIT ?`,
		string(model.StatusPending), time.Now().Unix(), limit,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list pending responses: %w", err)
	}
	defer rows.Close()

	var responses []*model.Response
	for rows.Next() {
		r, err := scanResponse(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan response: %w", err)
		}
		responses = append(responses, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Fetch each response's event so the shell can show what is being asked
	// rather than an opaque id.
	events := make([]*model.Event, 0, len(responses))
	for _, r := range responses {
		ev, err := s.Event(r.EventID)
		if err != nil {
			events = append(events, &model.Event{ID: r.EventID})
			continue
		}
		events = append(events, ev)
	}
	return responses, events, nil
}

// CountPendingResponses returns how many approvals are awaiting an answer.
func (s *Store) CountPendingResponses() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM responses WHERE status = ? AND expires_at > ?`,
		string(model.StatusPending), time.Now().Unix(),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending responses: %w", err)
	}
	return n, nil
}
