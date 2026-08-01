package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/router"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// Bounds on interactive response lifetimes, matching Hark.
const (
	minResponseTTL = 30 * time.Second
	maxResponseTTL = 24 * time.Hour
)

// notifyRequest is the POST /hooks/:token body. Only `body` is required.
type notifyRequest struct {
	Body      string              `json:"body"`
	Title     string              `json:"title"`
	ImageURL  string              `json:"imageUrl"`
	URL       string              `json:"url"`
	Priority  int                 `json:"priority"`
	Tags      []string            `json:"tags"`
	DeviceIDs []string            `json:"deviceIds"`
	Response  *model.ResponseSpec `json:"response"`
}

// responseView is the response object echoed back to the caller.
type responseView struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	CorrelationID string `json:"correlationId,omitempty"`
	Answer        string `json:"answer,omitempty"`
	AnsweredBy    string `json:"answeredBy,omitempty"`
	AnsweredAt    int64  `json:"answeredAt,omitempty"`
	ExpiresAt     int64  `json:"expiresAt"`
	// ApprovalURL is where a human answers. Anyone with this link can answer,
	// which is exactly how the notification tap-through works.
	ApprovalURL string `json:"approvalUrl"`
}

func (s *Server) responseView(r *model.Response) responseView {
	v := responseView{
		ID:            r.ID,
		Type:          string(r.Type),
		Status:        string(r.Status),
		CorrelationID: r.CorrelationID,
		Answer:        r.Answer,
		AnsweredBy:    r.AnsweredBy,
		ExpiresAt:     r.ExpiresAt.Unix(),
		ApprovalURL:   s.rt.ApprovalURL(r.Secret),
	}
	if r.AnsweredAt != nil {
		v.AnsweredAt = r.AnsweredAt.Unix()
	}
	return v
}

// handleNotify implements POST /hooks/:token.
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.resolveToken(w, r)
	if !ok {
		return
	}

	raw, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "%s", err)
		return
	}

	var req notifyRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err)
		return
	}
	if req.Body == "" {
		s.writeError(w, http.StatusBadRequest, "field %q is required", "body")
		return
	}

	var spec *model.ResponseSpec
	if req.Response != nil {
		spec = req.Response
		if !spec.Type.Valid() {
			s.writeError(w, http.StatusBadRequest,
				"response.type must be one of approval, yes_no, text (got %q)", spec.Type)
			return
		}
		if spec.ExpiresInSeconds != 0 {
			d := time.Duration(spec.ExpiresInSeconds) * time.Second
			if d < minResponseTTL || d > maxResponseTTL {
				s.writeError(w, http.StatusBadRequest,
					"response.expiresInSeconds must be between %d and %d",
					int(minResponseTTL.Seconds()), int(maxResponseTTL.Seconds()))
				return
			}
		}
	}

	// Idempotency: an identical replay returns the original result, a
	// conflicting one is rejected rather than silently diverging.
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if len(idemKey) > 200 {
			s.writeError(w, http.StatusBadRequest, "Idempotency-Key must be 1-200 characters")
			return
		}
		hash := store.HashPayload(raw)
		rec, err := s.store.LookupIdempotency(tok.ID, idemKey, hash)
		switch {
		case err == nil:
			s.replayIdempotent(w, rec)
			return
		case errors.Is(err, store.ErrIdempotencyConflict):
			s.writeError(w, http.StatusConflict,
				"Idempotency-Key %q was already used with a different payload", idemKey)
			return
		case !errors.Is(err, store.ErrNotFound):
			s.log.Error("idempotency lookup failed", "err", err)
			s.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	status, payload := s.publish(r, tok, &req, spec)

	if idemKey != "" {
		body, err := json.Marshal(payload)
		if err == nil {
			if err := s.store.SaveIdempotency(tok.ID, idemKey, store.HashPayload(raw), status, string(body)); err != nil {
				s.log.Warn("save idempotency failed", "err", err)
			}
		}
	}
	s.writeJSON(w, status, payload)
}

// notifyResult is the success body for a publish.
type notifyResult struct {
	OK         bool          `json:"ok"`
	EventID    string        `json:"eventId"`
	Delivered  int           `json:"delivered"`
	Idempotent bool          `json:"idempotent,omitempty"`
	Response   *responseView `json:"response,omitempty"`
	// Warning carries a non-fatal problem, e.g. an activity that had nowhere
	// to go. Silence here would look like success.
	Warning string `json:"warning,omitempty"`
}

// publish stores the event, creates any response, and delivers it.
func (s *Server) publish(r *http.Request, tok *model.Token, req *notifyRequest, spec *model.ResponseSpec) (int, any) {
	ev := &model.Event{
		AccountID: tok.AccountID,
		TokenID:   tok.ID,
		Title:     req.Title,
		Body:      req.Body,
		ImageURL:  req.ImageURL,
		URL:       req.URL,
		Priority:  req.Priority,
	}
	if ev.Priority == 0 {
		ev.Priority = 3
	}
	if err := s.store.CreateEvent(ev); err != nil {
		s.log.Error("create event failed", "err", err)
		return http.StatusInternalServerError, errorResponse{Error: "internal error"}
	}

	var resp *model.Response
	if spec != nil {
		ttl := s.cfg.DefaultResponseTTL
		if spec.ExpiresInSeconds != 0 {
			ttl = time.Duration(spec.ExpiresInSeconds) * time.Second
		}
		resp = &model.Response{
			EventID:       ev.ID,
			Type:          spec.Type,
			CorrelationID: spec.CorrelationID,
			Callback:      spec.Callback,
			ExpiresAt:     time.Now().Add(ttl).UTC(),
		}
		if err := s.store.CreateResponse(resp); err != nil {
			s.log.Error("create response failed", "err", err)
			return http.StatusInternalServerError, errorResponse{Error: "internal error"}
		}
	}

	delivered, err := s.rt.Deliver(r.Context(), ev, resp, req.DeviceIDs)
	if err != nil {
		switch {
		case errors.Is(err, router.ErrNoDevices):
			return http.StatusBadRequest, errorResponse{
				Error: "no enabled devices are registered; add one at " + s.cfg.BaseURL + "/subscribe",
			}
		case errors.Is(err, router.ErrAllFailed):
			// The event and response are still recorded, so a poller can see
			// what happened even though no push landed.
			out := notifyResult{OK: false, EventID: ev.ID, Delivered: 0, Warning: "all push targets failed"}
			if resp != nil {
				v := s.responseView(resp)
				out.Response = &v
			}
			return http.StatusBadGateway, out
		default:
			return http.StatusBadRequest, errorResponse{Error: err.Error()}
		}
	}

	if err := s.store.SetEventDelivered(ev.ID, delivered); err != nil {
		s.log.Warn("set delivered failed", "err", err)
	}

	out := notifyResult{OK: true, EventID: ev.ID, Delivered: delivered}
	if resp != nil {
		v := s.responseView(resp)
		out.Response = &v
	}
	return http.StatusOK, out
}

// replayIdempotent re-emits a stored result, flagged so the caller can tell it
// apart from a fresh delivery.
func (s *Server) replayIdempotent(w http.ResponseWriter, rec *store.IdempotentRecord) {
	var m map[string]any
	if err := json.Unmarshal([]byte(rec.Response), &m); err != nil {
		// Fall back to the stored bytes verbatim rather than failing a replay.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(rec.StatusCode)
		fmt.Fprint(w, rec.Response)
		return
	}
	m["idempotent"] = true
	s.writeJSON(w, rec.StatusCode, m)
}

// handleEventGet implements GET /hooks/:token/events/:eventId, the poll path
// for an interactive response.
func (s *Server) handleEventGet(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.resolveToken(w, r)
	if !ok {
		return
	}

	ev, err := s.store.Event(r.PathValue("eventId"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && ev.AccountID != tok.AccountID) {
		s.writeError(w, http.StatusNotFound, "unknown event")
		return
	}
	if err != nil {
		s.log.Error("event lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := notifyResult{OK: true, EventID: ev.ID, Delivered: ev.Delivered}
	resp, err := s.store.ResponseByEvent(ev.ID)
	if err == nil {
		v := s.responseView(resp)
		out.Response = &v
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("response lookup failed", "err", err)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleEventCancel implements POST /hooks/:token/events/:eventId/cancel.
func (s *Server) handleEventCancel(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.resolveToken(w, r)
	if !ok {
		return
	}

	ev, err := s.store.Event(r.PathValue("eventId"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && ev.AccountID != tok.AccountID) {
		s.writeError(w, http.StatusNotFound, "unknown event")
		return
	}
	if err != nil {
		s.log.Error("event lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp, err := s.store.ResponseByEvent(ev.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusBadRequest, "event has no interactive response to cancel")
		return
	}
	if err != nil {
		s.log.Error("response lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.store.CancelResponse(resp.ID); err != nil {
		if errors.Is(err, store.ErrAlreadyAnswered) {
			s.writeError(w, http.StatusConflict, "response is already %s", resp.Status)
			return
		}
		s.log.Error("cancel response failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp.Status = model.StatusCancelled
	v := s.responseView(resp)
	s.writeJSON(w, http.StatusOK, notifyResult{OK: true, EventID: ev.ID, Response: &v})
}
