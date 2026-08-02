package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/activity"
	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// livePageData drives templates/live.html.
type livePageData struct {
	Activity *model.Activity
	Progress int
	Native   bool
	BaseURL  string
}

// handleLivePage renders the live-updating view of an activity.
//
// On a client that can render a native Live Activity this page is a nicety. On
// every free transport it *is* the Live Activity: the notification's tap target
// is this URL, and the page streams state changes over SSE so a deploy can be
// watched in real time without a native app.
func (s *Server) handleLivePage(w http.ResponseWriter, r *http.Request) {
	act, err := s.store.Activity(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown activity", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("activity lookup failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	progress := 0
	if act.Progress != nil {
		progress = int(*act.Progress * 100)
	}
	s.render(w, "live.html", livePageData{
		Activity: act,
		Progress: progress,
		Native:   s.reg.SupportsLiveActivity(),
		BaseURL:  s.cfg.BaseURL,
	})
}

// handleLiveStream serves activity updates as Server-Sent Events.
func (s *Server) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	act, err := s.store.Activity(id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown activity", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("activity lookup failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Buffering proxies break SSE; this is the standard opt-out for nginx.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The request logger only records when a handler returns, and an SSE
	// connection is held open for the life of the activity — so without this a
	// live activity being watched is invisible in the logs.
	s.log.Info("sse stream opened", "activity", id, "watchers", s.hub.Watchers(id)+1)
	defer s.log.Info("sse stream closed", "activity", id)

	updates, cancel := s.hub.Subscribe(id)
	defer cancel()

	// Send current state immediately so a client that connects mid-flight is
	// correct without waiting for the next change.
	s.sendSSE(w, flusher, activityUpdate(act))

	// An already-ended activity has nothing further to stream.
	if act.State != model.ActivityActive {
		s.sendSSEEvent(w, flusher, "done", map[string]any{"activityId": id})
		return
	}

	// Keep-alives stop intermediaries from reaping an idle connection.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return

		case u, open := <-updates:
			if !open {
				return
			}
			s.sendSSE(w, flusher, u)
			if u.State != string(model.ActivityActive) {
				s.sendSSEEvent(w, flusher, "done", map[string]any{"activityId": id})
				return
			}

		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func activityUpdate(a *model.Activity) activity.Update {
	p := 0.0
	if a.Progress != nil {
		p = *a.Progress
	}
	return activity.Update{
		ActivityID: a.ID,
		Seq:        a.Seq,
		Title:      a.Title,
		Status:     a.Status,
		Progress:   p,
		State:      string(a.State),
		Style:      a.Style,
		Symbol:     a.Symbol,
		Accent:     a.AccentColor,
		UpdatedAt:  a.UpdatedAt.Unix(),
	}
}

func (s *Server) sendSSE(w http.ResponseWriter, f http.Flusher, u activity.Update) {
	s.sendSSEEvent(w, f, "update", u)
}

func (s *Server) sendSSEEvent(w http.ResponseWriter, f http.Flusher, event string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Warn("marshal sse payload failed", "err", err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	f.Flush()
}
