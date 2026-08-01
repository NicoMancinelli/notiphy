// Package api implements notiphy's HTTP surface: the Hark-compatible webhook
// endpoints, the approval and live-activity pages, device registration, and
// the dashboard.
package api

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/activity"
	"github.com/NicoMancinelli/notiphy/internal/callback"
	"github.com/NicoMancinelli/notiphy/internal/config"
	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/router"
	"github.com/NicoMancinelli/notiphy/internal/store"
	"github.com/NicoMancinelli/notiphy/internal/transport"
)

// webFS carries the templates and static assets, so the binary is the whole
// deployment — no asset directory to ship alongside it.
//
//go:embed all:templates all:static
var webFS embed.FS

// Server holds the dependencies shared by every handler.
type Server struct {
	cfg    config.Config
	store  *store.Store
	reg    *transport.Registry
	rt     *router.Router
	hub    *activity.Hub
	cb     *callback.Dispatcher
	log    *slog.Logger
	tmpl   *template.Template
	vapid  string // public key, served to the PWA at subscribe time
	static http.Handler
}

// New builds a Server and parses the embedded templates.
func New(
	cfg config.Config,
	st *store.Store,
	reg *transport.Registry,
	rt *router.Router,
	hub *activity.Hub,
	cb *callback.Dispatcher,
	vapidPublic string,
	log *slog.Logger,
) (*Server, error) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(webFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	staticFS, err := fsSub(webFS, "static")
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:    cfg,
		store:  st,
		reg:    reg,
		rt:     rt,
		hub:    hub,
		cb:     cb,
		log:    log,
		tmpl:   tmpl,
		vapid:  vapidPublic,
		static: http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))),
	}, nil
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- Hark-compatible webhook API ---
	mux.HandleFunc("POST /hooks/{token}", s.handleNotify)
	mux.HandleFunc("POST /hooks/{token}/live-activities", s.handleActivityStart)
	mux.HandleFunc("GET /hooks/{token}/live-activities/{id}", s.handleActivityGet)
	mux.HandleFunc("PATCH /hooks/{token}/live-activities/{id}", s.handleActivityUpdate)
	mux.HandleFunc("POST /hooks/{token}/live-activities/{id}", s.handleActivityUpdate)
	mux.HandleFunc("POST /hooks/{token}/live-activities/{id}/end", s.handleActivityEnd)
	mux.HandleFunc("GET /hooks/{token}/events/{eventId}", s.handleEventGet)
	mux.HandleFunc("POST /hooks/{token}/events/{eventId}/cancel", s.handleEventCancel)

	// --- Approval (capability URLs; the secret is the credential) ---
	mux.HandleFunc("GET /a/{secret}", s.handleApprovalPage)
	mux.HandleFunc("POST /a/{secret}", s.handleApprovalForm)
	mux.HandleFunc("POST /a/{secret}/{value}", s.handleApprovalDirect)
	mux.HandleFunc("GET /a/{secret}/{value}", s.handleApprovalDirect)

	// --- Live activity view ---
	mux.HandleFunc("GET /live/{id}", s.handleLivePage)
	mux.HandleFunc("GET /live/{id}/stream", s.handleLiveStream)

	// --- Operator surface. Everything below requires the admin token when one
	// is configured, because these endpoints add devices and mint credentials
	// rather than carrying a capability token of their own. ---
	admin := http.NewServeMux()
	admin.HandleFunc("GET /api/webpush/key", s.handleVAPIDKey)
	admin.HandleFunc("POST /api/webpush/subscribe", s.handleWebPushSubscribe)
	admin.HandleFunc("POST /api/devices", s.handleDeviceCreate)
	admin.HandleFunc("GET /api/devices", s.handleDeviceList)
	admin.HandleFunc("DELETE /api/devices/{id}", s.handleDeviceDelete)
	admin.HandleFunc("POST /api/devices/{id}/enable", s.handleDeviceEnable)
	admin.HandleFunc("POST /api/devices/{id}/disable", s.handleDeviceDisable)
	admin.HandleFunc("POST /api/devices/{id}/test", s.handleDeviceTest)
	admin.HandleFunc("GET /{$}", s.handleDashboard)
	admin.HandleFunc("GET /subscribe", s.handleSubscribePage)
	admin.HandleFunc("POST /dashboard/tokens", s.handleTokenCreate)
	admin.HandleFunc("POST /dashboard/tokens/{id}/revoke", s.handleTokenRevoke)

	guarded := s.requireAdmin(admin)
	for _, p := range []string{"/{$}", "/subscribe", "/api/", "/dashboard/"} {
		mux.Handle(p, guarded)
	}

	// --- Assets. The service worker must be served from the root so its scope
	// covers the whole origin; a /static/ path would silently limit it. ---
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /sw.js", s.handleServiceWorker)
	mux.HandleFunc("GET /manifest.webmanifest", s.handleManifest)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	return s.withLogging(mux)
}

// --- response helpers ---

// errorResponse is the JSON error body. It mirrors the success shape so
// clients can branch on `ok` alone.
type errorResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Warn("write json response failed", "err", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, format string, args ...any) {
	s.writeJSON(w, status, errorResponse{OK: false, Error: fmt.Sprintf(format, args...)})
}

// maxBody caps request bodies. Notification payloads are small; anything this
// large is a mistake or an attack.
const maxBody = 1 << 20 // 1 MiB

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxBody)
	}
	return b, nil
}

// resolveToken looks up the webhook token from the path. Unknown and revoked
// tokens are both 404, so a probe cannot distinguish them.
func (s *Server) resolveToken(w http.ResponseWriter, r *http.Request) (*model.Token, bool) {
	tok, err := s.store.TokenByValue(r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, "unknown token")
		return nil, false
	}
	if err != nil {
		s.log.Error("token lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	return tok, true
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render template failed", "template", name, "err", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"transports":         s.reg.Names(),
		"liveActivityNative": s.reg.SupportsLiveActivity(),
	})
}

// withLogging records method, path, status, and duration for every request.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Never log the token segment: it is the credential.
		path := r.URL.Path
		if strings.HasPrefix(path, "/hooks/") {
			path = redactSecondSegment(path, "/hooks/")
		} else if strings.HasPrefix(path, "/a/") {
			path = redactSecondSegment(path, "/a/")
		}

		s.log.Info("request",
			"method", r.Method,
			"path", path,
			"status", rec.status,
			"dur", time.Since(start).Round(time.Millisecond),
		)
	})
}

func redactSecondSegment(path, prefix string) string {
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.Index(rest, "/"); i >= 0 {
		return prefix + "***" + rest[i:]
	}
	return prefix + "***"
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE streaming still works through
// the logging wrapper.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
