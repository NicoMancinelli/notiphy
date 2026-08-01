package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// deviceView is the JSON representation of a registered device. Transport
// config is deliberately omitted: it holds push credentials.
type deviceView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Platform  string `json:"platform"`
	Disabled  bool   `json:"disabled"`
	CreatedAt int64  `json:"createdAt"`
	LastSeen  int64  `json:"lastSeen,omitempty"`
	// Capability flags let a caller see, per device, whether it can do
	// one-tap buttons or a native Live Activity.
	Buttons      bool `json:"buttons"`
	LiveActivity bool `json:"liveActivity"`
}

func (s *Server) deviceView(d *model.Device) deviceView {
	v := deviceView{
		ID:        d.ID,
		Name:      d.Name,
		Transport: d.Transport,
		Platform:  string(d.Platform),
		Disabled:  d.Disabled,
		CreatedAt: d.CreatedAt.Unix(),
	}
	if d.LastSeen != nil {
		v.LastSeen = d.LastSeen.Unix()
	}
	if t, ok := s.reg.Get(d.Transport); ok {
		c := t.Caps()
		v.Buttons, v.LiveActivity = c.Buttons, c.LiveActivity
	}
	return v
}

// handleVAPIDKey serves the public VAPID key the PWA needs to subscribe.
func (s *Server) handleVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if s.vapid == "" {
		s.writeError(w, http.StatusServiceUnavailable, "web push is not configured on this server")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"publicKey": s.vapid})
}

// webPushSubscribeRequest mirrors the browser's PushSubscription JSON.
type webPushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

// handleWebPushSubscribe registers (or refreshes) a PWA push subscription.
//
// Browsers rotate subscriptions silently — iOS in particular drops them after
// long inactivity — so a repeat subscribe for a known endpoint updates the
// existing device instead of accumulating duplicates.
func (s *Server) handleWebPushSubscribe(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	var req webPushSubscribeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err)
		return
	}
	if req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		s.writeError(w, http.StatusBadRequest, "endpoint, keys.p256dh and keys.auth are all required")
		return
	}

	cfg := map[string]string{
		"endpoint": req.Endpoint,
		"p256dh":   req.Keys.P256dh,
		"auth":     req.Keys.Auth,
	}

	if existing, err := s.store.DeviceByConfig("webpush", "endpoint", req.Endpoint); err == nil {
		if err := s.store.UpdateDeviceConfig(existing.ID, cfg); err != nil {
			s.log.Error("update webpush device failed", "err", err)
			s.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		// A re-subscribe from a device we previously disabled as "gone" means
		// it is back; re-enable it rather than leaving it silently dead.
		if existing.Disabled {
			if err := s.store.SetDeviceDisabled(existing.ID, false); err != nil {
				s.log.Warn("re-enable device failed", "device", existing.ID, "err", err)
			}
		}
		s.log.Info("web push subscription refreshed", "device", existing.ID)
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deviceId": existing.ID, "refreshed": true})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("device lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
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
		s.log.Error("create webpush device failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Info("web push device registered", "device", d.ID, "platform", platform)
	s.writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "deviceId": d.ID})
}

// deviceCreateRequest registers any transport's device.
type deviceCreateRequest struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Platform  string            `json:"platform"`
	Config    map[string]string `json:"config"`
}

// handleDeviceCreate registers a device for a named transport.
func (s *Server) handleDeviceCreate(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	var req deviceCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err)
		return
	}

	t, ok := s.reg.Get(req.Transport)
	if !ok {
		s.writeError(w, http.StatusBadRequest,
			"unknown transport %q; registered transports are: %s",
			req.Transport, strings.Join(s.reg.Names(), ", "))
		return
	}

	d := &model.Device{
		Name:      strings.TrimSpace(req.Name),
		Transport: req.Transport,
		Platform:  model.Platform(req.Platform),
		Config:    req.Config,
	}
	if d.Name == "" {
		d.Name = req.Transport + " device"
	}
	// Validate up front so a misconfigured device fails here rather than on
	// every future send.
	if err := t.Validate(d); err != nil {
		s.writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	if err := s.store.CreateDevice(d); err != nil {
		s.log.Error("create device failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.log.Info("device registered", "device", d.ID, "transport", d.Transport)
	s.writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "device": s.deviceView(d)})
}

// handleDeviceList returns all registered devices.
func (s *Server) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices(false)
	if err != nil {
		s.log.Error("list devices failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		out = append(out, s.deviceView(d))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "devices": out})
}

func (s *Server) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteDevice(r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, "unknown device")
			return
		}
		s.log.Error("delete device failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeviceEnable(w http.ResponseWriter, r *http.Request) {
	s.setDeviceDisabled(w, r, false)
}

func (s *Server) handleDeviceDisable(w http.ResponseWriter, r *http.Request) {
	s.setDeviceDisabled(w, r, true)
}

func (s *Server) setDeviceDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	if err := s.store.SetDeviceDisabled(r.PathValue("id"), disabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, "unknown device")
			return
		}
		s.log.Error("set device state failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": disabled})
}

// handleDeviceTest sends a test push to one device, so registration can be
// confirmed without wiring up a webhook first.
func (s *Server) handleDeviceTest(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.Device(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, "unknown device")
		return
	}
	if err != nil {
		s.log.Error("device lookup failed", "err", err)
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	t, ok := s.reg.Get(d.Transport)
	if !ok {
		s.writeError(w, http.StatusBadRequest, "transport %q is not registered", d.Transport)
		return
	}

	n := &model.Notification{
		Title:    "notiphy",
		Body:     "Test notification — your device is wired up correctly.",
		Priority: 3,
		URL:      s.cfg.BaseURL,
	}
	if err := t.Send(r.Context(), d, n); err != nil {
		s.log.Warn("test push failed", "device", d.ID, "err", err)
		s.writeError(w, http.StatusBadGateway, "test push failed: %s", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
