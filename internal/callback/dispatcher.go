// Package callback delivers response outcomes back to the caller's endpoint.
//
// When a caller attaches a callback to an interactive response, they are
// asking to be told the answer rather than to poll for it. Delivery is durable:
// attempts are queued in SQLite and retried across restarts, because the whole
// point is that the caller's CI job or agent can stop waiting.
package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// backoff is the delay before each retry. Five attempts spanning immediate to
// one hour, matching Hark's documented behaviour.
var backoff = []time.Duration{
	0,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	time.Hour,
}

// MaxAttempts is how many times a callback is tried before being abandoned.
var MaxAttempts = len(backoff)

// Payload is the JSON body POSTed to the caller's callback URL.
type Payload struct {
	EventID       string `json:"eventId"`
	ResponseID    string `json:"responseId"`
	CorrelationID string `json:"correlationId,omitempty"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	Answer        string `json:"answer,omitempty"`
	AnsweredBy    string `json:"answeredBy,omitempty"`
	AnsweredAt    int64  `json:"answeredAt,omitempty"`
}

// Dispatcher drains the callback queue.
type Dispatcher struct {
	store  *store.Store
	client *http.Client
	ua     string
	log    *slog.Logger
}

// New constructs a Dispatcher.
func New(st *store.Store, userAgent string, log *slog.Logger) *Dispatcher {
	if userAgent == "" {
		userAgent = "notiphy/1.0"
	}
	return &Dispatcher{
		store:  st,
		client: &http.Client{Timeout: 20 * time.Second},
		ua:     userAgent,
		log:    log,
	}
}

// Enqueue queues the callback for a freshly answered response.
func (d *Dispatcher) Enqueue(r *model.Response) error {
	if r.Callback == nil || r.Callback.URL == "" {
		return nil
	}

	p := Payload{
		EventID:       r.EventID,
		ResponseID:    r.ID,
		CorrelationID: r.CorrelationID,
		Type:          string(r.Type),
		Status:        string(r.Status),
		Answer:        r.Answer,
		AnsweredBy:    r.AnsweredBy,
	}
	if r.AnsweredAt != nil {
		p.AnsweredAt = r.AnsweredAt.Unix()
	}

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal callback payload: %w", err)
	}
	return d.store.EnqueueCallback(r.ID, r.Callback.URL, r.Callback.Token, string(body))
}

// Run drains the queue on an interval until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.drain(ctx); err != nil {
				d.log.Warn("callback drain failed", "err", err)
			}
		}
	}
}

func (d *Dispatcher) drain(ctx context.Context) error {
	due, err := d.store.DueCallbacks(20)
	if err != nil {
		return err
	}
	for _, c := range due {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		d.attempt(ctx, c)
	}
	return nil
}

func (d *Dispatcher) attempt(ctx context.Context, c *store.PendingCallback) {
	err := d.post(ctx, c)
	if err == nil {
		if cerr := d.store.CompleteCallback(c.ID); cerr != nil {
			d.log.Warn("complete callback failed", "callback", c.ID, "err", cerr)
		}
		return
	}

	attempts := c.Attempts + 1
	giveUp := attempts >= MaxAttempts

	next := time.Now()
	if !giveUp {
		next = next.Add(backoff[attempts])
	}

	if giveUp {
		d.log.Warn("callback abandoned after final attempt",
			"callback", c.ID, "url", c.URL, "attempts", attempts, "err", err)
	} else {
		d.log.Info("callback attempt failed, will retry",
			"callback", c.ID, "attempt", attempts, "next", next, "err", err)
	}

	if ferr := d.store.FailCallback(c.ID, attempts, next, err.Error(), giveUp); ferr != nil {
		d.log.Warn("record callback failure failed", "callback", c.ID, "err", ferr)
	}
}

func (d *Dispatcher) post(ctx context.Context, c *store.PendingCallback) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader([]byte(c.Payload)))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.ua)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("callback endpoint returned %d", resp.StatusCode)
	}
	return nil
}
