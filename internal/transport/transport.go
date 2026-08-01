// Package transport delivers notifications to devices.
//
// This file defines the contract every delivery mechanism implements. It is
// the load-bearing abstraction in notiphy: the HTTP API, the store, and the
// router are all written against Transport, so adding APNs later (and with it
// Live Activities, Dynamic Island, and native action buttons) is a matter of
// registering one more implementation — no caller changes.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// ErrNotSupported is returned by a transport asked to do something outside its
// capabilities. The router checks Caps() first, so seeing this means a bug.
var ErrNotSupported = errors.New("operation not supported by this transport")

// Transport delivers notifications to one kind of device.
type Transport interface {
	// Name is the stable identifier stored on devices ("ntfy", "webpush", "apns").
	Name() string

	// Caps reports what this transport can render. The router reads it to
	// decide how much of a request must be degraded before sending.
	Caps() model.Caps

	// Send delivers a single notification. The notification has already been
	// adapted by the router to fit Caps().
	Send(ctx context.Context, d *model.Device, n *model.Notification) error

	// Validate checks that a device's config carries what this transport needs,
	// so registration fails loudly rather than every later send failing quietly.
	Validate(d *model.Device) error
}

// ActivityTransport is implemented by transports that can render a stateful
// Live Activity natively. Transports that cannot simply don't implement it,
// and the router falls back to the live web page plus milestone notifications.
type ActivityTransport interface {
	Transport
	StartActivity(ctx context.Context, d *model.Device, a *model.Activity) error
	UpdateActivity(ctx context.Context, d *model.Device, a *model.Activity) error
	EndActivity(ctx context.Context, d *model.Device, a *model.Activity) error
}

// Registry holds the transports available at runtime.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Transport
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]Transport)}
}

// Register adds a transport, replacing any prior one with the same name.
func (r *Registry) Register(t Transport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[t.Name()] = t
}

// Get looks up a transport by name.
func (r *Registry) Get(name string) (Transport, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.m[name]
	return t, ok
}

// Names returns the registered transport names, richest capabilities first.
// The dashboard uses this ordering to show the best available path.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		si, sj := r.m[names[i]].Caps().Score(), r.m[names[j]].Caps().Score()
		if si != sj {
			return si > sj
		}
		return names[i] < names[j]
	})
	return names
}

// SupportsLiveActivity reports whether any registered transport can render a
// Live Activity natively. When false, the server tells the user plainly that
// activities are rendered as a live web page instead.
func (r *Registry) SupportsLiveActivity() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.m {
		if t.Caps().LiveActivity {
			return true
		}
	}
	return false
}

// httpClient is shared by the HTTP-based transports. The timeout is
// deliberately short: a push that has not left in 15s is not worth blocking
// the publish request for.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// httpError describes a non-2xx push response.
type httpError struct {
	Transport string
	Status    int
	Body      string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s: upstream returned %d: %s", e.Transport, e.Status, e.Body)
}

// Gone reports whether the upstream says this device is permanently invalid,
// which is the signal to disable it rather than retry forever. Web Push uses
// 404/410 for expired subscriptions.
func (e *httpError) Gone() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusGone
}

// IsGone reports whether err indicates a permanently dead device.
func IsGone(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.Gone()
	}
	return false
}
