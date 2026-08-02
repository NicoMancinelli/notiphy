package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/store"
	"github.com/NicoMancinelli/notiphy/internal/transport"
)

// pushBackoff is the delay before each retry attempt. Short at first, because
// most failures are a transient network blip and an approval waiting on the
// other end is time-sensitive.
var pushBackoff = []time.Duration{
	0,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
}

// MaxPushAttempts counts the first try plus every queued retry.
var MaxPushAttempts = len(pushBackoff)

// enqueueRetry queues a failed push for another attempt.
//
// Permanently dead targets are not retried: a Web Push subscription that
// returned 410 will never come back, and the device is disabled instead.
func (r *Router) enqueueRetry(d *model.Device, n *model.Notification, eventID, activityID string, cause error) {
	if transport.IsGone(cause) {
		return
	}

	payload, err := json.Marshal(n)
	if err != nil {
		r.log.Warn("cannot queue push retry: marshal failed", "device", d.ID, "err", err)
		return
	}
	if err := r.store.EnqueuePush(d.ID, eventID, activityID, string(payload), pushBackoff[1]); err != nil {
		r.log.Warn("cannot queue push retry", "device", d.ID, "err", err)
		return
	}
	r.log.Info("queued push for retry", "device", d.ID, "event", eventID, "in", pushBackoff[1])
}

// RunRetries drains the push retry queue until ctx is cancelled.
func (r *Router) RunRetries(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.drainRetries(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.log.Warn("push retry drain failed", "err", err)
			}
		}
	}
}

func (r *Router) drainRetries(ctx context.Context) error {
	due, err := r.store.DuePushes(20)
	if err != nil {
		return err
	}
	for _, p := range due {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		r.retryOne(ctx, p)
	}
	return nil
}

func (r *Router) retryOne(ctx context.Context, p *store.QueuedPush) {
	// Stop retrying a question that has already been settled — re-pushing an
	// approval someone answered from another device is worse than dropping it.
	if p.EventID != "" {
		if resp, err := r.store.ResponseByEvent(p.EventID); err == nil && resp.Terminal() {
			r.log.Info("dropping queued push: response already settled",
				"event", p.EventID, "status", resp.Status)
			if err := r.store.CompletePush(p.ID); err != nil {
				r.log.Warn("complete push failed", "push", p.ID, "err", err)
			}
			return
		}
	}

	d, err := r.store.Device(p.DeviceID)
	if err != nil {
		r.log.Info("dropping queued push: device is gone", "device", p.DeviceID)
		r.store.CompletePush(p.ID)
		return
	}
	if d.Disabled {
		r.log.Info("dropping queued push: device is disabled", "device", p.DeviceID)
		r.store.CompletePush(p.ID)
		return
	}

	t, ok := r.reg.Get(d.Transport)
	if !ok {
		r.store.CompletePush(p.ID)
		return
	}

	var n model.Notification
	if err := json.Unmarshal([]byte(p.Payload), &n); err != nil {
		r.log.Warn("dropping queued push: payload is unreadable", "push", p.ID, "err", err)
		r.store.CompletePush(p.ID)
		return
	}

	sendErr := t.Send(ctx, d, &n)
	if sendErr == nil {
		r.log.Info("queued push delivered on retry", "device", d.ID, "attempt", p.Attempts+1)
		r.record(p.EventID, p.ActivityID, d, true, "")
		if err := r.store.CompletePush(p.ID); err != nil {
			r.log.Warn("complete push failed", "push", p.ID, "err", err)
		}
		if err := r.store.TouchDevice(d.ID); err != nil {
			r.log.Warn("touch device failed", "device", d.ID, "err", err)
		}
		return
	}

	attempts := p.Attempts + 1
	giveUp := attempts >= MaxPushAttempts || transport.IsGone(sendErr)

	next := time.Now()
	if !giveUp {
		next = next.Add(pushBackoff[attempts])
	}

	if giveUp {
		r.log.Warn("giving up on push after final attempt",
			"device", d.ID, "attempts", attempts, "err", sendErr)
		r.record(p.EventID, p.ActivityID, d, false, fmt.Sprintf("gave up after %d attempts: %v", attempts, sendErr))
		if transport.IsGone(sendErr) {
			if derr := r.store.SetDeviceDisabled(d.ID, true); derr != nil {
				r.log.Warn("disable device failed", "device", d.ID, "err", derr)
			}
		}
	}

	if err := r.store.FailPush(p.ID, attempts, next, sendErr.Error(), giveUp); err != nil {
		r.log.Warn("record push failure failed", "push", p.ID, "err", err)
	}
}
