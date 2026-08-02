// Package router decides how each device receives a notification.
//
// It is where notiphy's central promise is kept: callers write one Hark-shaped
// payload, and the router adapts it to whatever the target device can actually
// render. A caller asking for an approval gets native buttons on a transport
// that has them and a tap-through approval page on one that does not, without
// changing the request.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/NicoMancinelli/notiphy/internal/activity"
	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/store"
	"github.com/NicoMancinelli/notiphy/internal/transport"
)

// Router delivers events and activities to devices.
type Router struct {
	store        *store.Store
	reg          *transport.Registry
	hub          *activity.Hub
	baseURL      string
	progressStep float64
	log          *slog.Logger
}

// New constructs a Router.
func New(st *store.Store, reg *transport.Registry, hub *activity.Hub, baseURL string, progressStep float64, log *slog.Logger) *Router {
	if progressStep <= 0 {
		progressStep = 0.25
	}
	return &Router{
		store:        st,
		reg:          reg,
		hub:          hub,
		baseURL:      strings.TrimRight(baseURL, "/"),
		progressStep: progressStep,
		log:          log,
	}
}

// ErrNoDevices is returned when nothing is registered to deliver to. Callers
// surface it as a 400 with an actionable message rather than a silent success,
// because "sent to nobody" is never what the caller meant.
var ErrNoDevices = errors.New("no enabled devices are registered")

// ErrAllFailed is returned when every push target failed. Hark answers 502 here.
var ErrAllFailed = errors.New("all push targets failed")

// ApprovalURL is the page a human opens to answer a response. Knowing the
// secret is what authorizes answering, so this link is the capability.
func (r *Router) ApprovalURL(secret string) string {
	return r.baseURL + "/a/" + secret
}

// AnswerURL is the direct-answer endpoint used by native action buttons.
func (r *Router) AnswerURL(secret, value string) string {
	return r.baseURL + "/a/" + secret + "/" + value
}

// LiveURL is the live-updating web view for an activity.
func (r *Router) LiveURL(activityID string) string {
	return r.baseURL + "/live/" + activityID
}

// targets resolves the devices an event should reach. An empty deviceIDs means
// every enabled device.
func (r *Router) targets(deviceIDs []string) ([]*model.Device, error) {
	devices, err := r.store.ListDevices(true)
	if err != nil {
		return nil, err
	}
	if len(deviceIDs) == 0 {
		return devices, nil
	}

	want := make(map[string]bool, len(deviceIDs))
	for _, id := range deviceIDs {
		want[id] = true
	}
	var out []*model.Device
	for _, d := range devices {
		if want[d.ID] {
			out = append(out, d)
			delete(want, d.ID)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for id := range want {
			missing = append(missing, id)
		}
		return nil, fmt.Errorf("unknown or disabled device(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// adapt rewrites a notification to fit one transport's capabilities. This is
// the degradation step, and it is deliberately additive: we never remove
// information, we only add the fallback path a weaker client needs.
func (r *Router) adapt(base model.Notification, caps model.Caps, resp *model.Response) model.Notification {
	n := base

	if resp != nil {
		// Every transport gets the tap target, including ones with buttons —
		// tapping the notification body should always lead somewhere useful.
		n.URL = r.ApprovalURL(resp.Secret)
		n.Response = resp

		// Attach buttons regardless. Transports that render them resolve the
		// approval in one tap; the rest ignore the field harmlessly.
		for _, c := range resp.Type.Choices() {
			n.Actions = append(n.Actions, model.Action{
				Label:  c.Label,
				Value:  c.Value,
				URL:    r.AnswerURL(resp.Secret, c.Value),
				Method: "POST",
			})
		}

		// Text replies need a keyboard, so a client without inline reply must
		// be told to open the page rather than look for a button that is not there.
		if resp.Type == model.ResponseText && !caps.InlineReply {
			n.Body = strings.TrimSpace(n.Body) + "\n\nTap to reply."
		}
	}

	if n.ImageURL != "" && !caps.Images {
		n.ImageURL = ""
	}
	if n.Priority == 0 {
		n.Priority = 3
	}
	return n
}

// Deliver pushes an event to its target devices and returns the number of
// accepted pushes. Delivery is concurrent because one slow transport should
// not delay the others.
func (r *Router) Deliver(ctx context.Context, ev *model.Event, resp *model.Response, deviceIDs []string) (int, error) {
	devices, err := r.targets(deviceIDs)
	if err != nil {
		return 0, err
	}
	if len(devices) == 0 {
		return 0, ErrNoDevices
	}

	base := model.Notification{
		EventID:  ev.ID,
		Title:    ev.Title,
		Body:     ev.Body,
		ImageURL: ev.ImageURL,
		URL:      ev.URL,
		Priority: ev.Priority,
	}

	// Badge with everything currently awaiting an answer, not just this one,
	// so the icon reflects the real backlog.
	if n, err := r.store.CountPendingResponses(); err == nil {
		base.BadgeCount = &n
	} else {
		r.log.Warn("count pending responses failed", "err", err)
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		delivered int
		attempted int
	)

	for _, d := range devices {
		t, ok := r.reg.Get(d.Transport)
		if !ok {
			r.log.Warn("skipping device with unregistered transport",
				"device", d.ID, "transport", d.Transport)
			r.record(ev.ID, "", d, false, "transport not registered")
			continue
		}
		attempted++

		wg.Add(1)
		go func(d *model.Device, t transport.Transport) {
			defer wg.Done()

			n := r.adapt(base, t.Caps(), resp)
			err := t.Send(ctx, d, &n)

			mu.Lock()
			if err == nil {
				delivered++
			}
			mu.Unlock()

			r.afterSend(ev.ID, "", d, err)
			if err != nil {
				// A transient failure must not silently drop the push: an
				// unanswered approval leaves an agent hanging until timeout.
				r.enqueueRetry(d, &n, ev.ID, "", err)
			}
		}(d, t)
	}
	wg.Wait()

	if attempted > 0 && delivered == 0 {
		return 0, ErrAllFailed
	}
	return delivered, nil
}

// afterSend records the outcome and reacts to a permanently dead device by
// disabling it, so a stale Web Push subscription stops being retried forever.
func (r *Router) afterSend(eventID, activityID string, d *model.Device, err error) {
	if err == nil {
		r.record(eventID, activityID, d, true, "")
		if terr := r.store.TouchDevice(d.ID); terr != nil {
			r.log.Warn("touch device failed", "device", d.ID, "err", terr)
		}
		return
	}

	r.log.Warn("push failed", "device", d.ID, "transport", d.Transport, "err", err)
	r.record(eventID, activityID, d, false, err.Error())

	if transport.IsGone(err) {
		r.log.Info("disabling device: upstream reports it is permanently gone",
			"device", d.ID, "transport", d.Transport)
		if derr := r.store.SetDeviceDisabled(d.ID, true); derr != nil {
			r.log.Warn("disable device failed", "device", d.ID, "err", derr)
		}
	}
}

func (r *Router) record(eventID, activityID string, d *model.Device, ok bool, errMsg string) {
	if err := r.store.RecordDelivery(&model.Delivery{
		EventID:    eventID,
		ActivityID: activityID,
		DeviceID:   d.ID,
		Transport:  d.Transport,
		OK:         ok,
		Error:      errMsg,
	}); err != nil {
		r.log.Warn("record delivery failed", "device", d.ID, "err", err)
	}
}

// ActivityPhase distinguishes the three points in an activity's life, because
// each has a different notification policy.
type ActivityPhase string

const (
	PhaseStart  ActivityPhase = "start"
	PhaseUpdate ActivityPhase = "update"
	PhaseEnd    ActivityPhase = "end"
)

// DeliverActivity pushes an activity state change.
//
// Transports that render Live Activities natively receive every update, since
// updating a card in place is free. Transports that cannot are throttled to
// milestones — start, end, a status change, or a progress jump of at least
// progressStep — because on those clients every update is a separate
// notification, and a deploy that reports 1% at a time would be unusable.
func (r *Router) DeliverActivity(ctx context.Context, act *model.Activity, phase ActivityPhase) (int, error) {
	r.publishToHub(act)

	devices, err := r.targets(nil)
	if err != nil {
		return 0, err
	}
	if len(devices) == 0 {
		// An activity with no devices is not an error: its state is still
		// recorded and the live web view still works.
		return 0, nil
	}

	notifyDegraded := r.shouldNotify(act, phase)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		delivered int
	)

	for _, d := range devices {
		t, ok := r.reg.Get(d.Transport)
		if !ok {
			continue
		}

		wg.Add(1)
		go func(d *model.Device, t transport.Transport) {
			defer wg.Done()

			var err error
			if at, native := t.(transport.ActivityTransport); native && t.Caps().LiveActivity {
				switch phase {
				case PhaseStart:
					err = at.StartActivity(ctx, d, act)
				case PhaseUpdate:
					err = at.UpdateActivity(ctx, d, act)
				case PhaseEnd:
					err = at.EndActivity(ctx, d, act)
				}
			} else {
				if !notifyDegraded {
					return // throttled; the live page already reflects the change
				}
				n := r.activityNotification(act, phase, t.Caps())
				err = t.Send(ctx, d, &n)
			}

			mu.Lock()
			if err == nil {
				delivered++
			}
			mu.Unlock()

			r.afterSend("", act.ID, d, err)
		}(d, t)
	}
	wg.Wait()

	if notifyDegraded {
		p := 0.0
		if act.Progress != nil {
			p = *act.Progress
		}
		if err := r.store.MarkActivityNotified(act.ID, p, act.Status); err != nil {
			r.log.Warn("mark activity notified failed", "activity", act.ID, "err", err)
		}
	}
	return delivered, nil
}

// shouldNotify implements the milestone throttle for non-native transports.
func (r *Router) shouldNotify(act *model.Activity, phase ActivityPhase) bool {
	if phase == PhaseStart || phase == PhaseEnd {
		return true
	}
	if act.Status != "" && act.Status != act.LastNotifiedStatus {
		return true
	}
	if act.Progress == nil {
		return false
	}
	return math.Abs(*act.Progress-act.LastNotifiedProgress) >= r.progressStep
}

// activityNotification renders an activity as a plain notification for clients
// that cannot show a Live Activity, pointing at the live web view.
func (r *Router) activityNotification(act *model.Activity, phase ActivityPhase, caps model.Caps) model.Notification {
	title := act.Title
	if title == "" {
		title = "Activity"
	}

	body := act.Status
	if act.Progress != nil {
		pct := int(math.Round(*act.Progress * 100))
		if body != "" {
			body = fmt.Sprintf("%s · %d%%", body, pct)
		} else {
			body = fmt.Sprintf("%d%%", pct)
		}
	}
	if body == "" {
		body = string(phase)
	}

	priority := 3
	if phase == PhaseEnd {
		priority = 4
	}

	n := model.Notification{
		// Sharing the event ID across an activity's pushes lets clients that
		// support tags replace the previous card instead of stacking.
		EventID:  "act-" + act.ID,
		Title:    title,
		Body:     body,
		URL:      r.LiveURL(act.ID),
		Priority: priority,
	}
	if act.Symbol != "" {
		n.Tags = []string{act.Symbol}
	}
	return n
}

func (r *Router) publishToHub(act *model.Activity) {
	p := 0.0
	if act.Progress != nil {
		p = *act.Progress
	}
	r.hub.Publish(activity.Update{
		ActivityID: act.ID,
		Seq:        act.Seq,
		Title:      act.Title,
		Status:     act.Status,
		Progress:   p,
		State:      string(act.State),
		Style:      act.Style,
		Symbol:     act.Symbol,
		Accent:     act.AccentColor,
		UpdatedAt:  act.UpdatedAt.Unix(),
	})
}
