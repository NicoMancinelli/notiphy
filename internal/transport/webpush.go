package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/NicoMancinelli/notiphy/internal/model"
	webpush "github.com/SherClockHolmes/webpush-go"
)

// WebPush delivers to a PWA installed on the home screen via the W3C Web Push
// protocol, signed with VAPID.
//
// This is the only free iOS path with no third-party in the loop: Safari
// registers the subscription against Apple's push service directly, and your
// server signs the delivery. It requires no Apple Developer account.
//
// Its ceiling is real, though. WebKit silently ignores the notification
// `actions` array, so one-tap approve/deny does not render on iPhone (Android
// Chrome does honour it), and the web platform has no Live Activity API at all.
type WebPush struct {
	PublicKey  string
	PrivateKey string
	Subject    string
}

// NewWebPush constructs the Web Push transport.
func NewWebPush(publicKey, privateKey, subject string) *WebPush {
	if subject == "" {
		subject = "mailto:admin@localhost"
	}
	return &WebPush{PublicKey: publicKey, PrivateKey: privateKey, Subject: subject}
}

// GenerateVAPIDKeys returns a fresh (private, public) VAPID keypair. Called
// once on first boot and persisted; rotating it invalidates every existing
// subscription, so it is never regenerated automatically.
func GenerateVAPIDKeys() (private string, public string, err error) {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", fmt.Errorf("generate VAPID keys: %w", err)
	}
	return priv, pub, nil
}

// Name identifies this transport.
func (w *WebPush) Name() string { return "webpush" }

// Caps reports Web Push capabilities, assuming the weaker (WebKit) client.
func (w *WebPush) Caps() model.Caps {
	return model.Caps{
		Buttons:      false,
		LiveActivity: false,
		Images:       true,
		InlineReply:  false,
		Update:       true, // a shared notification tag replaces the prior one
	}
}

// Validate requires the three values a browser hands back at subscribe time.
func (w *WebPush) Validate(d *model.Device) error {
	for _, k := range []string{"endpoint", "p256dh", "auth"} {
		if strings.TrimSpace(d.Config[k]) == "" {
			return fmt.Errorf("webpush device requires a %q config value", k)
		}
	}
	return nil
}

// webPushPayload is the JSON the service worker receives. Keep it in sync with
// web/static/sw.js.
type webPushPayload struct {
	Title    string          `json:"title"`
	Body     string          `json:"body"`
	Icon     string          `json:"icon,omitempty"`
	Image    string          `json:"image,omitempty"`
	URL      string          `json:"url,omitempty"`
	Tag      string          `json:"tag,omitempty"`
	Actions  []webPushAction `json:"actions,omitempty"`
	Priority int             `json:"priority,omitempty"`
}

type webPushAction struct {
	Action string `json:"action"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Method string `json:"method,omitempty"`
	Body   string `json:"body,omitempty"`
}

// Send encrypts and delivers a push to the device's subscription.
func (w *WebPush) Send(ctx context.Context, d *model.Device, notif *model.Notification) error {
	if w.PrivateKey == "" || w.PublicKey == "" {
		return fmt.Errorf("webpush: VAPID keys are not configured")
	}

	payload := webPushPayload{
		Title:    notif.Title,
		Body:     notif.Body,
		Image:    notif.ImageURL,
		URL:      notif.URL,
		Priority: notif.Priority,
	}
	if payload.Title == "" {
		payload.Title = "notiphy"
	}
	// Grouping activity pushes under one tag lets a later update replace the
	// earlier card instead of stacking a new one.
	if notif.EventID != "" {
		payload.Tag = notif.EventID
	}

	// Chrome/Android honours these; WebKit drops them. Sending them anyway
	// costs nothing and upgrades the experience where it is supported.
	for i, a := range notif.Actions {
		if i >= 2 { // the web platform renders at most two reliably
			break
		}
		payload.Actions = append(payload.Actions, webPushAction{
			Action: a.Value,
			Title:  a.Label,
			URL:    a.URL,
			Method: a.Method,
			Body:   a.Body,
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webpush: marshal payload: %w", err)
	}

	sub := &webpush.Subscription{
		Endpoint: d.Config["endpoint"],
		Keys: webpush.Keys{
			P256dh: d.Config["p256dh"],
			Auth:   d.Config["auth"],
		},
	}

	urgency := webpush.UrgencyNormal
	if notif.Priority >= 4 {
		urgency = webpush.UrgencyHigh
	}

	resp, err := webpush.SendNotificationWithContext(ctx, body, sub, &webpush.Options{
		Subscriber:      w.Subject,
		VAPIDPublicKey:  w.PublicKey,
		VAPIDPrivateKey: w.PrivateKey,
		TTL:             60 * 60 * 24,
		Urgency:         urgency,
	})
	if err != nil {
		return fmt.Errorf("webpush: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// 404/410 mean the subscription is dead; the caller disables the device
		// rather than retrying a target that will never come back.
		return &httpError{Transport: "webpush", Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	return nil
}
