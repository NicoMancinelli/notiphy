// Package activity provides the in-process fan-out that powers the live
// activity web view. When no transport can render a native Live Activity, this
// is what makes /live/:id update in real time instead of requiring a reload.
package activity

import "sync"

// Update is one state change broadcast to watchers of an activity.
type Update struct {
	ActivityID string  `json:"activityId"`
	Seq        int     `json:"seq"`
	Title      string  `json:"title"`
	Status     string  `json:"status"`
	Progress   float64 `json:"progress"`
	State      string  `json:"state"`
	Style      string  `json:"style"`
	Symbol     string  `json:"symbol"`
	Accent     string  `json:"accent"`
	UpdatedAt  int64   `json:"updatedAt"`
}

// Hub fans updates out to SSE subscribers, keyed by activity ID.
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[chan Update]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[chan Update]struct{})}
}

// Subscribe registers a watcher for one activity. The returned cancel function
// must be called to release the subscription.
func (h *Hub) Subscribe(activityID string) (<-chan Update, func()) {
	// Buffered so a slow reader cannot block the publishing request; if the
	// buffer fills we drop rather than stall the API.
	ch := make(chan Update, 8)

	h.mu.Lock()
	if h.subs[activityID] == nil {
		h.subs[activityID] = make(map[chan Update]struct{})
	}
	h.subs[activityID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			if m := h.subs[activityID]; m != nil {
				delete(m, ch)
				if len(m) == 0 {
					delete(h.subs, activityID)
				}
			}
			h.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Publish broadcasts an update to every watcher of that activity. Watchers
// that cannot keep up miss intermediate frames; since each frame carries the
// full state, the next one they receive is still correct.
func (h *Hub) Publish(u Update) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[u.ActivityID] {
		select {
		case ch <- u:
		default:
		}
	}
}

// Watchers reports how many subscribers an activity currently has.
func (h *Hub) Watchers(activityID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[activityID])
}
