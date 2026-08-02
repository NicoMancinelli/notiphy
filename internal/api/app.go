package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// appCookie holds the PWA's capability token inside the installed app's own
// cookie jar, which iOS keeps separate from Safari's.
const appCookie = "notiphy_app"

// requireApp gates the PWA shell and its API.
//
// The token arrives once via ?t= in the manifest's start_url — baked in at
// install time, because a cookie set while browsing in Safari never reaches
// the installed app. After that it lives in the app's own cookie jar.
//
// When no admin token is configured the whole server is open anyway, so this
// degrades to a no-op rather than inventing a barrier that protects nothing.
func (s *Server) requireApp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		if t := r.URL.Query().Get("t"); t != "" {
			if _, err := s.store.AppTokenByValue(t); err == nil {
				http.SetCookie(w, &http.Cookie{
					Name:     appCookie,
					Value:    t,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
					Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
					MaxAge:   60 * 60 * 24 * 365 * 5,
				})
				clean := *r.URL
				q := clean.Query()
				q.Del("t")
				clean.RawQuery = q.Encode()
				http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
				return
			}
		}

		if c, err := r.Cookie(appCookie); err == nil {
			if _, err := s.store.AppTokenByValue(c.Value); err == nil {
				next.ServeHTTP(w, r)
				return
			}
		}

		// An admin session may also open the app directly, which is what
		// happens when you preview it from the dashboard in a desktop browser.
		if s.adminAuthorized(r) {
			next.ServeHTTP(w, r)
			return
		}

		if acceptsHTML(r) {
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, "unauthorized.html", map[string]any{"BaseURL": s.cfg.BaseURL})
			return
		}
		s.writeError(w, http.StatusUnauthorized, "this app install is not authorized; re-add it from %s/subscribe", s.cfg.BaseURL)
	})
}

// appShellData drives templates/app.html.
type appShellData struct {
	BaseURL      string
	WebPushReady bool
	NativeLive   bool
}

// handleAppShell serves the installed PWA's home screen.
//
// This is the manifest's start_url. It is deliberately not the dashboard: the
// dashboard mints webhook tokens, and a phone that is merely receiving
// notifications has no business holding that power.
func (s *Server) handleAppShell(w http.ResponseWriter, r *http.Request) {
	s.render(w, "app.html", appShellData{
		BaseURL:      s.cfg.BaseURL,
		WebPushReady: s.vapid != "",
		NativeLive:   s.reg.SupportsLiveActivity(),
	})
}

// appApproval is one pending question, flattened for the shell.
type appApproval struct {
	Title       string `json:"title"`
	Body        string `json:"body"`
	Type        string `json:"type"`
	Secret      string `json:"secret"`
	ExpiresAt   int64  `json:"expiresAt"`
	CreatedAt   int64  `json:"createdAt"`
	ApprovalURL string `json:"approvalUrl"`
}

// appActivity is one running activity, flattened for the shell.
type appActivity struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	State    string  `json:"state"`
	LiveURL  string  `json:"liveUrl"`
}

// appState is what the shell polls for.
type appState struct {
	OK           bool          `json:"ok"`
	Pending      []appApproval `json:"pending"`
	Activities   []appActivity `json:"activities"`
	PendingCount int           `json:"pendingCount"`
	Subscribed   bool          `json:"subscribed"`
}

// handleAppState returns everything the shell renders: what needs answering,
// what is running, and the badge count.
func (s *Server) handleAppState(w http.ResponseWriter, r *http.Request) {
	responses, events, err := s.store.PendingResponses(25)
	if err != nil {
		s.log.Error("list pending responses failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := appState{OK: true, Pending: make([]appApproval, 0, len(responses))}
	for i, resp := range responses {
		a := appApproval{
			Type:        string(resp.Type),
			Secret:      resp.Secret,
			ExpiresAt:   resp.ExpiresAt.Unix(),
			CreatedAt:   resp.CreatedAt.Unix(),
			ApprovalURL: s.rt.ApprovalURL(resp.Secret),
		}
		if i < len(events) && events[i] != nil {
			a.Title, a.Body = events[i].Title, events[i].Body
		}
		if a.Title == "" {
			a.Title = "Request"
		}
		out.Pending = append(out.Pending, a)
	}
	out.PendingCount = len(out.Pending)

	activities, err := s.store.ListActivities(10)
	if err != nil {
		s.log.Error("list activities failed", "err", err)
	}
	out.Activities = make([]appActivity, 0, len(activities))
	for _, act := range activities {
		if act.State != model.ActivityActive {
			continue
		}
		p := 0.0
		if act.Progress != nil {
			p = *act.Progress
		}
		out.Activities = append(out.Activities, appActivity{
			ID: act.ID, Title: act.Title, Status: act.Status,
			Progress: p, State: string(act.State), LiveURL: s.rt.LiveURL(act.ID),
		})
	}

	// Tell the shell whether this install still has a live subscription, so it
	// can re-register silently rather than waiting for a push that never comes.
	if c, err := r.Cookie(appCookie); err == nil {
		if t, err := s.store.AppTokenByValue(c.Value); err == nil && t.DeviceID != "" {
			if d, err := s.store.Device(t.DeviceID); err == nil {
				out.Subscribed = !d.Disabled
			}
		}
	}

	s.writeJSON(w, http.StatusOK, out)
}

// handleAppManifest serves the PWA manifest with a freshly minted app token in
// start_url.
//
// This is the whole reason the manifest is dynamic. iOS captures start_url at
// "Add to Home Screen" time and gives the installed app a cookie jar of its
// own, so the token has to travel inside the URL — there is no other channel
// from Safari into the installed app.
func (s *Server) handleAppManifest(w http.ResponseWriter, r *http.Request) {
	raw, err := webFS.ReadFile("static/manifest.webmanifest")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		s.log.Error("parse manifest failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	start := "/app"
	if s.cfg.AdminToken != "" {
		// Only mint a token for a request that is already authorized —
		// otherwise fetching the manifest would be a way to grant yourself access.
		if s.adminAuthorized(r) {
			if t, err := s.store.CreateAppToken("home screen install"); err == nil {
				start = "/app?t=" + t.Token
				s.log.Info("minted app token for a PWA install", "token", t.ID)
			} else {
				s.log.Error("mint app token failed", "err", err)
			}
		}
	}
	m["start_url"] = start

	body, err := json.Marshal(m)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	// Never cache: each install should capture its own token.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// handleAppSubscribe registers this install's push subscription and binds it to
// the app token, so re-opening the app can verify its own subscription.
func (s *Server) handleAppSubscribe(w http.ResponseWriter, r *http.Request) {
	deviceID, status, err := s.upsertWebPushDevice(r)
	if err != nil {
		s.writeError(w, status, "%s", err)
		return
	}

	if c, cerr := r.Cookie(appCookie); cerr == nil {
		if t, terr := s.store.AppTokenByValue(c.Value); terr == nil {
			if err := s.store.BindAppTokenDevice(t.ID, deviceID); err != nil {
				s.log.Warn("bind app token to device failed", "err", err)
			}
		}
	}
	s.writeJSON(w, status, map[string]any{"ok": true, "deviceId": deviceID})
}

// upsertWebPushDevice creates or refreshes a Web Push device from a browser
// PushSubscription. Shared by the Safari and installed-PWA paths.
func (s *Server) upsertWebPushDevice(r *http.Request) (string, int, error) {
	raw, err := readBody(r)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	var req webPushSubscribeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("invalid JSON body: %s", err)
	}
	if req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		return "", http.StatusBadRequest, fmt.Errorf("endpoint, keys.p256dh and keys.auth are all required")
	}

	cfg := map[string]string{
		"endpoint": req.Endpoint,
		"p256dh":   req.Keys.P256dh,
		"auth":     req.Keys.Auth,
	}

	existing, err := s.store.DeviceByConfig("webpush", "endpoint", req.Endpoint)
	switch {
	case err == nil:
		if err := s.store.UpdateDeviceConfig(existing.ID, cfg); err != nil {
			return "", http.StatusInternalServerError, fmt.Errorf("internal error")
		}
		if existing.Disabled {
			if err := s.store.SetDeviceDisabled(existing.ID, false); err != nil {
				s.log.Warn("re-enable device failed", "device", existing.ID, "err", err)
			}
		}
		return existing.ID, http.StatusOK, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", http.StatusInternalServerError, fmt.Errorf("internal error")
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Web Push device"
	}
	platform := model.Platform(req.Platform)
	if platform == "" {
		platform = model.PlatformWeb
	}

	d := &model.Device{Name: name, Transport: "webpush", Platform: platform, Config: cfg}
	if err := s.store.CreateDevice(d); err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("internal error")
	}
	s.log.Info("web push device registered", "device", d.ID, "platform", platform)
	return d.ID, http.StatusCreated, nil
}
