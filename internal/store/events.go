package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// CreateEvent stores a publish request.
func (s *Store) CreateEvent(e *model.Event) error {
	if e.ID == "" {
		e.ID = model.NewID("evt")
	}
	e.CreatedAt = time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO events (id, account_id, token_id, title, body, image_url, url, priority, delivered, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AccountID, e.TokenID, e.Title, e.Body, e.ImageURL, e.URL, e.Priority, e.Delivered, e.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	return nil
}

// SetEventDelivered records how many pushes were accepted for an event.
func (s *Store) SetEventDelivered(id string, n int) error {
	_, err := s.db.Exec(`UPDATE events SET delivered = ? WHERE id = ?`, n, id)
	return err
}

// Event looks up one event.
func (s *Store) Event(id string) (*model.Event, error) {
	var (
		e       model.Event
		created int64
	)
	err := s.db.QueryRow(
		`SELECT id, account_id, token_id, title, body, image_url, url, priority, delivered, created_at
		 FROM events WHERE id = ?`, id,
	).Scan(&e.ID, &e.AccountID, &e.TokenID, &e.Title, &e.Body, &e.ImageURL, &e.URL, &e.Priority, &e.Delivered, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup event: %w", err)
	}
	e.CreatedAt = fromUnix(created)
	return &e, nil
}

// ListEvents returns recent events for the dashboard, newest first.
func (s *Store) ListEvents(limit int) ([]*model.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, account_id, token_id, title, body, image_url, url, priority, delivered, created_at
		 FROM events WHERE account_id = ? ORDER BY created_at DESC LIMIT ?`,
		DefaultAccountID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []*model.Event
	for rows.Next() {
		var (
			e       model.Event
			created int64
		)
		if err := rows.Scan(&e.ID, &e.AccountID, &e.TokenID, &e.Title, &e.Body, &e.ImageURL, &e.URL, &e.Priority, &e.Delivered, &created); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.CreatedAt = fromUnix(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// --- responses ---

// CreateResponse stores a pending interactive response.
func (s *Store) CreateResponse(r *model.Response) error {
	if r.ID == "" {
		r.ID = model.NewID("rsp")
	}
	if r.Secret == "" {
		r.Secret = model.NewID("s")
	}
	r.CreatedAt = time.Now().UTC()
	r.Status = model.StatusPending

	var cbURL, cbToken string
	if r.Callback != nil {
		cbURL, cbToken = r.Callback.URL, r.Callback.Token
	}

	_, err := s.db.Exec(
		`INSERT INTO responses (id, event_id, type, status, correlation_id, answer, answered_by,
		                        callback_url, callback_token, secret, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?)`,
		r.ID, r.EventID, string(r.Type), string(r.Status), r.CorrelationID,
		cbURL, cbToken, r.Secret, r.ExpiresAt.Unix(), r.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("create response: %w", err)
	}
	return nil
}

const responseCols = `id, event_id, type, status, correlation_id, answer, answered_by,
	callback_url, callback_token, secret, expires_at, answered_at, created_at`

func scanResponse(sc interface{ Scan(...any) error }) (*model.Response, error) {
	var (
		r                model.Response
		typ, status      string
		cbURL, cbToken   string
		expires, created int64
		answeredAt       sql.NullInt64
	)
	if err := sc.Scan(&r.ID, &r.EventID, &typ, &status, &r.CorrelationID, &r.Answer, &r.AnsweredBy,
		&cbURL, &cbToken, &r.Secret, &expires, &answeredAt, &created); err != nil {
		return nil, err
	}
	r.Type = model.ResponseType(typ)
	r.Status = model.ResponseStatus(status)
	if cbURL != "" {
		r.Callback = &model.Callback{URL: cbURL, Token: cbToken}
	}
	r.ExpiresAt = fromUnix(expires)
	r.AnsweredAt = fromNullUnix(answeredAt)
	r.CreatedAt = fromUnix(created)
	return &r, nil
}

// ResponseByEvent finds the response attached to an event, if any.
func (s *Store) ResponseByEvent(eventID string) (*model.Response, error) {
	row := s.db.QueryRow(`SELECT `+responseCols+` FROM responses WHERE event_id = ?`, eventID)
	r, err := scanResponse(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup response: %w", err)
	}
	return s.expireIfDue(r)
}

// ResponseBySecret resolves the capability token embedded in an approval link.
func (s *Store) ResponseBySecret(secret string) (*model.Response, error) {
	row := s.db.QueryRow(`SELECT `+responseCols+` FROM responses WHERE secret = ?`, secret)
	r, err := scanResponse(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup response by secret: %w", err)
	}
	return s.expireIfDue(r)
}

// expireIfDue lazily transitions a pending response past its deadline. Doing
// this on read means expiry is correct even if the background sweeper is not
// running, so pollers never see a stale "pending".
func (s *Store) expireIfDue(r *model.Response) (*model.Response, error) {
	if r.Status != model.StatusPending || time.Now().Before(r.ExpiresAt) {
		return r, nil
	}
	if _, err := s.db.Exec(
		`UPDATE responses SET status = ? WHERE id = ? AND status = ?`,
		string(model.StatusExpired), r.ID, string(model.StatusPending),
	); err != nil {
		return nil, fmt.Errorf("expire response: %w", err)
	}
	r.Status = model.StatusExpired
	return r, nil
}

// ErrAlreadyAnswered is returned when answering a response that is no longer
// pending. The first answer wins; later taps are rejected rather than silently
// overwriting, which matters when a notification lands on several devices.
var ErrAlreadyAnswered = errors.New("response is no longer pending")

// AnswerResponse records an answer, but only if the response is still pending
// and unexpired. The conditional UPDATE makes this atomic against concurrent
// taps from multiple devices.
func (s *Store) AnswerResponse(id, answer, answeredBy string) (*model.Response, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`UPDATE responses SET status = ?, answer = ?, answered_by = ?, answered_at = ?
		 WHERE id = ? AND status = ? AND expires_at > ?`,
		string(model.StatusAnswered), answer, answeredBy, now.Unix(),
		id, string(model.StatusPending), now.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("answer response: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrAlreadyAnswered
	}

	row := s.db.QueryRow(`SELECT `+responseCols+` FROM responses WHERE id = ?`, id)
	r, err := scanResponse(row)
	if err != nil {
		return nil, fmt.Errorf("reload response: %w", err)
	}
	return r, nil
}

// CancelResponse withdraws a pending response.
func (s *Store) CancelResponse(id string) error {
	res, err := s.db.Exec(
		`UPDATE responses SET status = ? WHERE id = ? AND status = ?`,
		string(model.StatusCancelled), id, string(model.StatusPending),
	)
	if err != nil {
		return fmt.Errorf("cancel response: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlreadyAnswered
	}
	return nil
}

// ExpireDueResponses sweeps responses past their deadline. Returns the count.
func (s *Store) ExpireDueResponses() (int, error) {
	res, err := s.db.Exec(
		`UPDATE responses SET status = ? WHERE status = ? AND expires_at <= ?`,
		string(model.StatusExpired), string(model.StatusPending), time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("expire responses: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- deliveries ---

// RecordDelivery logs one push attempt.
func (s *Store) RecordDelivery(d *model.Delivery) error {
	if d.ID == "" {
		d.ID = model.NewID("dly")
	}
	ok := 0
	if d.OK {
		ok = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO deliveries (id, event_id, activity_id, device_id, transport, ok, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.EventID, d.ActivityID, d.DeviceID, d.Transport, ok, d.Error, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	return nil
}

// DeliveriesForEvent returns the per-device outcomes for an event.
func (s *Store) DeliveriesForEvent(eventID string) ([]*model.Delivery, error) {
	rows, err := s.db.Query(
		`SELECT id, event_id, activity_id, device_id, transport, ok, error, created_at
		 FROM deliveries WHERE event_id = ? ORDER BY created_at ASC`, eventID,
	)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()

	var out []*model.Delivery
	for rows.Next() {
		var (
			d       model.Delivery
			ok      int
			created int64
		)
		if err := rows.Scan(&d.ID, &d.EventID, &d.ActivityID, &d.DeviceID, &d.Transport, &ok, &d.Error, &created); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		d.OK = ok == 1
		d.CreatedAt = fromUnix(created)
		out = append(out, &d)
	}
	return out, rows.Err()
}
