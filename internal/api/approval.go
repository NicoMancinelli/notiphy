package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// approvalPageData drives templates/approval.html.
type approvalPageData struct {
	Response *model.Response
	Event    *model.Event
	Choices  []model.Choice
	IsText   bool
	Done     bool
	Message  string
	BaseURL  string
}

// handleApprovalPage renders the page a human lands on after tapping a
// notification. This is the fallback that keeps approvals working on clients
// with no action-button support — every iPhone, on every free transport.
func (s *Server) handleApprovalPage(w http.ResponseWriter, r *http.Request) {
	resp, ok := s.loadResponse(w, r, true)
	if !ok {
		return
	}

	data := approvalPageData{
		Response: resp,
		Choices:  resp.Type.Choices(),
		IsText:   resp.Type == model.ResponseText,
		Done:     resp.Terminal(),
		BaseURL:  s.cfg.BaseURL,
	}
	if ev, err := s.store.Event(resp.EventID); err == nil {
		data.Event = ev
	}
	if data.Done {
		data.Message = terminalMessage(resp)
	}
	s.render(w, "approval.html", data)
}

// handleApprovalForm handles a submission from the approval page.
func (s *Server) handleApprovalForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid form submission")
		return
	}
	value := r.FormValue("value")
	if text := strings.TrimSpace(r.FormValue("text")); text != "" {
		value = text
	}
	s.answer(w, r, value, "web")
}

// handleApprovalDirect handles a one-tap answer, which is what a native action
// button hits: POST /a/{secret}/{value}.
func (s *Server) handleApprovalDirect(w http.ResponseWriter, r *http.Request) {
	s.answer(w, r, r.PathValue("value"), "notification")
}

// loadResponse resolves the capability secret from the path.
func (s *Server) loadResponse(w http.ResponseWriter, r *http.Request, html bool) (*model.Response, bool) {
	resp, err := s.store.ResponseBySecret(r.PathValue("secret"))
	if errors.Is(err, store.ErrNotFound) {
		if html {
			w.WriteHeader(http.StatusNotFound)
			s.render(w, "approval.html", approvalPageData{
				Done:    true,
				Message: "This approval link is not valid. It may have been withdrawn.",
				BaseURL: s.cfg.BaseURL,
			})
		} else {
			s.writeError(w, http.StatusNotFound, "unknown or expired approval link")
		}
		return nil, false
	}
	if err != nil {
		s.log.Error("response lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	return resp, true
}

// answer records a decision and fires any callback.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, value, by string) {
	wantsHTML := acceptsHTML(r)

	resp, ok := s.loadResponse(w, r, wantsHTML)
	if !ok {
		return
	}

	value = strings.TrimSpace(value)
	if !validAnswer(resp.Type, value) {
		s.finish(w, r, resp, http.StatusBadRequest,
			"That is not a valid answer for this request.", wantsHTML)
		return
	}

	answered, err := s.store.AnswerResponse(resp.ID, value, by)
	if errors.Is(err, store.ErrAlreadyAnswered) {
		// Re-tapping is common — the same notification can land on several
		// devices. Report the settled state rather than treating it as an error.
		current, lerr := s.store.ResponseBySecret(resp.Secret)
		if lerr == nil {
			resp = current
		}
		s.finish(w, r, resp, http.StatusConflict, terminalMessage(resp), wantsHTML)
		return
	}
	if err != nil {
		s.log.Error("answer response failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.cb.Enqueue(answered); err != nil {
		s.log.Warn("enqueue callback failed", "response", answered.ID, "err", err)
	}
	s.log.Info("response answered", "response", answered.ID, "answer", answered.Answer, "by", by)

	s.finish(w, r, answered, http.StatusOK, terminalMessage(answered), wantsHTML)
}

// finish writes the outcome as HTML for a browser or JSON for a button tap.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, resp *model.Response, status int, msg string, wantsHTML bool) {
	if wantsHTML {
		data := approvalPageData{
			Response: resp,
			Choices:  resp.Type.Choices(),
			IsText:   resp.Type == model.ResponseText,
			Done:     true,
			Message:  msg,
			BaseURL:  s.cfg.BaseURL,
		}
		if ev, err := s.store.Event(resp.EventID); err == nil {
			data.Event = ev
		}
		w.WriteHeader(status)
		s.render(w, "approval.html", data)
		return
	}
	v := s.responseView(resp)
	s.writeJSON(w, status, map[string]any{"ok": status == http.StatusOK, "response": v, "message": msg})
}

func validAnswer(t model.ResponseType, value string) bool {
	if value == "" {
		return false
	}
	choices := t.Choices()
	if choices == nil { // text: any non-empty answer
		return len(value) <= 2000
	}
	for _, c := range choices {
		if c.Value == value {
			return true
		}
	}
	return false
}

func terminalMessage(r *model.Response) string {
	switch r.Status {
	case model.StatusAnswered:
		switch r.Answer {
		case "approve":
			return "Approved."
		case "deny":
			return "Denied."
		case "yes":
			return "Answered: yes."
		case "no":
			return "Answered: no."
		default:
			return "Reply sent."
		}
	case model.StatusExpired:
		return "This request expired before it was answered."
	case model.StatusCancelled:
		return "This request was withdrawn by the sender."
	default:
		return ""
	}
}

// acceptsHTML distinguishes a human tapping through from a notification action
// button firing an HTTP request in the background.
func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
