package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/router"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// Bounds on activity lifetimes, matching Hark.
const (
	minActivityTTL = 60 * time.Second
	maxActivityTTL = 8 * time.Hour
	maxStaleAfter  = 8 * time.Hour
)

// activityStartRequest is the POST /hooks/:token/live-activities body.
type activityStartRequest struct {
	Key              string   `json:"key"`
	Title            string   `json:"title"`
	Status           string   `json:"status"`
	Progress         *float64 `json:"progress"`
	Symbol           string   `json:"symbol"`
	AccentColor      string   `json:"accentColor"`
	Style            string   `json:"style"`
	ExpiresInSeconds int      `json:"expiresInSeconds"`
	StaleInSeconds   int      `json:"staleInSeconds"`
	Replace          bool     `json:"replace"`
}

// activityPatchRequest is a merge patch. Pointer fields distinguish "absent"
// from "set to zero", which is what makes PATCH merge rather than replace.
type activityPatchRequest struct {
	Title       *string  `json:"title"`
	Status      *string  `json:"status"`
	Progress    *float64 `json:"progress"`
	Symbol      *string  `json:"symbol"`
	AccentColor *string  `json:"accentColor"`
	Style       *string  `json:"style"`
}

// activityView is the activity representation returned to callers.
type activityView struct {
	OK        bool     `json:"ok"`
	ID        string   `json:"id"`
	Key       string   `json:"key,omitempty"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Progress  *float64 `json:"progress,omitempty"`
	Style     string   `json:"style"`
	State     string   `json:"state"`
	Seq       int      `json:"seq"`
	ExpiresAt int64    `json:"expiresAt"`
	StaleAt   int64    `json:"staleAt"`
	Delivered int      `json:"delivered"`
	// LiveURL is the live-updating web view. On a client that cannot render a
	// native Live Activity this link is the Live Activity.
	LiveURL string `json:"liveUrl"`
	// Native reports whether any registered transport rendered this as a real
	// Live Activity. False means it was delivered as notifications plus the
	// web view, and the caller deserves to know which they got.
	Native  bool   `json:"native"`
	Warning string `json:"warning,omitempty"`
}

func (s *Server) activityView(a *model.Activity, delivered int) activityView {
	return activityView{
		OK:        true,
		ID:        a.ID,
		Key:       a.Key,
		Title:     a.Title,
		Status:    a.Status,
		Progress:  a.Progress,
		Style:     a.Style,
		State:     string(a.State),
		Seq:       a.Seq,
		ExpiresAt: a.ExpiresAt.Unix(),
		StaleAt:   a.StaleAt.Unix(),
		Delivered: delivered,
		LiveURL:   s.rt.LiveURL(a.ID),
		Native:    s.reg.SupportsLiveActivity(),
	}
}

func validProgress(p *float64) bool {
	return p == nil || (*p >= 0 && *p <= 1)
}

// handleActivityStart implements POST /hooks/:token/live-activities.
func (s *Server) handleActivityStart(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.resolveToken(w, r)
	if !ok {
		return
	}

	raw, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	var req activityStartRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err)
		return
	}

	if req.Title == "" {
		s.writeError(w, http.StatusBadRequest, "field %q is required", "title")
		return
	}
	if !validProgress(req.Progress) {
		s.writeError(w, http.StatusBadRequest, "progress must be between 0 and 1")
		return
	}
	if req.Style != "" && !model.ValidStyles[req.Style] {
		s.writeError(w, http.StatusBadRequest, "unknown style %q", req.Style)
		return
	}

	ttl := s.cfg.DefaultActivityTTL
	if req.ExpiresInSeconds != 0 {
		ttl = time.Duration(req.ExpiresInSeconds) * time.Second
		if ttl < minActivityTTL || ttl > maxActivityTTL {
			s.writeError(w, http.StatusBadRequest,
				"expiresInSeconds must be between %d and %d",
				int(minActivityTTL.Seconds()), int(maxActivityTTL.Seconds()))
			return
		}
	}
	stale := s.cfg.DefaultStaleAfter
	if req.StaleInSeconds != 0 {
		stale = time.Duration(req.StaleInSeconds) * time.Second
		if stale < 0 || stale > maxStaleAfter {
			s.writeError(w, http.StatusBadRequest,
				"staleInSeconds must be between 0 and %d", int(maxStaleAfter.Seconds()))
			return
		}
	}

	now := time.Now().UTC()
	act := &model.Activity{
		AccountID:   tok.AccountID,
		TokenID:     tok.ID,
		Key:         req.Key,
		Title:       req.Title,
		Status:      req.Status,
		Progress:    req.Progress,
		Symbol:      req.Symbol,
		AccentColor: req.AccentColor,
		Style:       req.Style,
		ExpiresAt:   now.Add(ttl),
		StaleAt:     now.Add(stale),
	}

	if err := s.store.CreateActivity(act, req.Replace); err != nil {
		if errors.Is(err, store.ErrActivityConflict) {
			s.writeError(w, http.StatusConflict,
				"an active activity already exists for key %q; pass replace:true to displace it", req.Key)
			return
		}
		s.log.Error("create activity failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	delivered, err := s.rt.DeliverActivity(r.Context(), act, router.PhaseStart)
	if err != nil {
		s.log.Warn("activity start delivery failed", "activity", act.ID, "err", err)
	}

	view := s.activityView(act, delivered)
	if !view.Native {
		view.Warning = "no transport can render a native Live Activity; delivered as notifications plus the live web view"
	}
	s.writeJSON(w, http.StatusCreated, view)
}

// loadActivity resolves an activity scoped to the token's account.
func (s *Server) loadActivity(w http.ResponseWriter, r *http.Request, accountID string) (*model.Activity, bool) {
	act, err := s.store.Activity(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && act.AccountID != accountID) {
		s.writeError(w, http.StatusNotFound, "unknown activity")
		return nil, false
	}
	if err != nil {
		s.log.Error("activity lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	return act, true
}

// handleActivityGet implements GET /hooks/:token/live-activities/:id.
func (s *Server) handleActivityGet(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.resolveToken(w, r)
	if !ok {
		return
	}
	act, ok := s.loadActivity(w, r, tok.AccountID)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, s.activityView(act, 0))
}

// handleActivityUpdate implements PATCH (and POST) on a live activity.
func (s *Server) handleActivityUpdate(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.resolveToken(w, r)
	if !ok {
		return
	}
	act, ok := s.loadActivity(w, r, tok.AccountID)
	if !ok {
		return
	}
	if act.State != model.ActivityActive {
		s.writeError(w, http.StatusConflict, "activity has already ended")
		return
	}

	raw, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	var req activityPatchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err)
		return
	}
	if !validProgress(req.Progress) {
		s.writeError(w, http.StatusBadRequest, "progress must be between 0 and 1")
		return
	}
	if req.Style != nil && !model.ValidStyles[*req.Style] {
		s.writeError(w, http.StatusBadRequest, "unknown style %q", *req.Style)
		return
	}

	updated, err := s.store.UpdateActivity(act.ID, store.ActivityPatch{
		Title:       req.Title,
		Status:      req.Status,
		Progress:    req.Progress,
		Symbol:      req.Symbol,
		AccentColor: req.AccentColor,
		Style:       req.Style,
	})
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusConflict, "activity has already ended")
		return
	}
	if err != nil {
		s.log.Error("update activity failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	delivered, err := s.rt.DeliverActivity(r.Context(), updated, router.PhaseUpdate)
	if err != nil {
		s.log.Warn("activity update delivery failed", "activity", updated.ID, "err", err)
	}
	s.writeJSON(w, http.StatusOK, s.activityView(updated, delivered))
}

// activityEndRequest optionally sets the final state.
type activityEndRequest struct {
	Status   string   `json:"status"`
	Progress *float64 `json:"progress"`
}

// handleActivityEnd implements POST /hooks/:token/live-activities/:id/end.
func (s *Server) handleActivityEnd(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.resolveToken(w, r)
	if !ok {
		return
	}
	act, ok := s.loadActivity(w, r, tok.AccountID)
	if !ok {
		return
	}
	if act.State != model.ActivityActive {
		s.writeError(w, http.StatusConflict, "activity has already ended")
		return
	}

	// An end with no body is valid, so a read failure here is not fatal.
	var req activityEndRequest
	if raw, err := readBody(r); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err)
			return
		}
	}
	if !validProgress(req.Progress) {
		s.writeError(w, http.StatusBadRequest, "progress must be between 0 and 1")
		return
	}

	if err := s.store.EndActivity(act.ID, req.Status, req.Progress); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusConflict, "activity has already ended")
			return
		}
		s.log.Error("end activity failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	ended, err := s.store.Activity(act.ID)
	if err != nil {
		s.log.Error("reload activity failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	delivered, err := s.rt.DeliverActivity(r.Context(), ended, router.PhaseEnd)
	if err != nil {
		s.log.Warn("activity end delivery failed", "activity", ended.ID, "err", err)
	}
	s.writeJSON(w, http.StatusOK, s.activityView(ended, delivered))
}
