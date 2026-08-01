package api

import (
	"net/http"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// dashDevice pairs a device with the capabilities its transport reports, so
// the dashboard can show per-device whether one-tap buttons and native Live
// Activities are actually available.
type dashDevice struct {
	*model.Device
	Buttons      bool
	LiveActivity bool
}

// dashboardData drives templates/dashboard.html.
type dashboardData struct {
	BaseURL      string
	Tokens       []*model.Token
	Devices      []dashDevice
	Events       []*model.Event
	Activities   []*model.Activity
	Transports   []string
	NativeLive   bool
	WebPushReady bool
}

// handleDashboard renders the operator view: tokens, devices, recent events,
// and activities.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := dashboardData{
		BaseURL:      s.cfg.BaseURL,
		Transports:   s.reg.Names(),
		NativeLive:   s.reg.SupportsLiveActivity(),
		WebPushReady: s.vapid != "",
	}

	var err error
	if data.Tokens, err = s.store.ListTokens(); err != nil {
		s.log.Error("list tokens failed", "err", err)
	}
	devices, err := s.store.ListDevices(false)
	if err != nil {
		s.log.Error("list devices failed", "err", err)
	}
	for _, d := range devices {
		row := dashDevice{Device: d}
		if t, ok := s.reg.Get(d.Transport); ok {
			c := t.Caps()
			row.Buttons, row.LiveActivity = c.Buttons, c.LiveActivity
		}
		data.Devices = append(data.Devices, row)
	}
	if data.Events, err = s.store.ListEvents(25); err != nil {
		s.log.Error("list events failed", "err", err)
	}
	if data.Activities, err = s.store.ListActivities(10); err != nil {
		s.log.Error("list activities failed", "err", err)
	}

	s.render(w, "dashboard.html", data)
}

// subscribeData drives templates/subscribe.html.
type subscribeData struct {
	BaseURL      string
	WebPushReady bool
	NtfyServer   string
}

// handleSubscribePage renders the page a phone visits to register itself.
func (s *Server) handleSubscribePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "subscribe.html", subscribeData{
		BaseURL:      s.cfg.BaseURL,
		WebPushReady: s.vapid != "",
		NtfyServer:   s.cfg.NtfyDefaultServer,
	})
}

// handleTokenCreate mints a webhook token from the dashboard form.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.store.CreateToken(r.FormValue("name")); err != nil {
		s.log.Error("create token failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleTokenRevoke revokes a webhook token from the dashboard form.
func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeToken(r.PathValue("id")); err != nil {
		s.log.Error("revoke token failed", "err", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleServiceWorker serves the service worker from the origin root.
//
// Scope matters here: a worker served from /static/ can only control /static/,
// so push events for the rest of the site would never reach it. Serving it at
// / is what makes the PWA able to receive notifications at all.
func (s *Server) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	body, err := webFS.ReadFile("static/sw.js")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
}

// handleIcon serves the app icon for favicon and apple-touch-icon requests.
// A PNG is returned even for /favicon.ico: every current browser accepts one,
// and it saves carrying a second image just for the legacy extension.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	body, err := webFS.ReadFile("static/icon-192.png")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(body)
}

// handleManifest serves the PWA manifest. iOS only grants Web Push to a site
// added to the Home Screen, and it will not offer that without a manifest.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	body, err := webFS.ReadFile("static/manifest.webmanifest")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	w.Write(body)
}
