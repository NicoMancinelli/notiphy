package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// Ntfy delivers through an ntfy server — your own, or ntfy.sh.
//
// Capability note: ntfy's Android client renders action buttons natively, so an
// approval resolves in one tap. Its iOS client does not render action buttons
// at all, so on iPhone the router instead points the notification's tap target
// at the approval page. Caps() reports Buttons=false because the router must
// assume the weaker case; the buttons we attach are a bonus that Android picks
// up and iOS ignores.
type Ntfy struct {
	// DefaultServer is used by devices that do not name their own.
	DefaultServer string
}

// NewNtfy constructs the ntfy transport.
func NewNtfy(defaultServer string) *Ntfy {
	if defaultServer == "" {
		defaultServer = "https://ntfy.sh"
	}
	return &Ntfy{DefaultServer: strings.TrimRight(defaultServer, "/")}
}

// Name identifies this transport.
func (n *Ntfy) Name() string { return "ntfy" }

// Caps reports ntfy's capabilities, assuming the weaker (iOS) client. Images
// come through as attachments; nothing here can update in place or render a
// Live Activity.
func (n *Ntfy) Caps() model.Caps {
	return model.Caps{
		Buttons:      false,
		LiveActivity: false,
		Images:       true,
		InlineReply:  false,
		Update:       false,
	}
}

// Validate requires a topic — without one there is nowhere to publish.
func (n *Ntfy) Validate(d *model.Device) error {
	if strings.TrimSpace(d.Config["topic"]) == "" {
		return fmt.Errorf("ntfy device requires a %q config value", "topic")
	}
	return nil
}

func (n *Ntfy) server(d *model.Device) string {
	if s := strings.TrimSpace(d.Config["server"]); s != "" {
		return strings.TrimRight(s, "/")
	}
	return n.DefaultServer
}

// ntfyMessage is the publish payload. Field names follow ntfy's JSON API.
type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title,omitempty"`
	Message  string       `json:"message"`
	Priority int          `json:"priority,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Click    string       `json:"click,omitempty"`
	Attach   string       `json:"attach,omitempty"`
	Icon     string       `json:"icon,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

type ntfyAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
}

// Send publishes a notification to the device's topic.
func (n *Ntfy) Send(ctx context.Context, d *model.Device, notif *model.Notification) error {
	msg := ntfyMessage{
		Topic:    d.Config["topic"],
		Title:    notif.Title,
		Message:  notif.Body,
		Priority: notif.Priority,
		Tags:     notif.Tags,
		Click:    notif.URL,
		Attach:   notif.ImageURL,
	}
	if msg.Message == "" {
		// ntfy rejects an empty message; a blank push is never what was meant.
		msg.Message = notif.Title
	}

	// ntfy caps action buttons at three. Android renders these; iOS ignores
	// them and falls back to the Click target the router already set.
	for i, a := range notif.Actions {
		if i >= 3 {
			break
		}
		method := a.Method
		if method == "" {
			method = http.MethodPost
		}
		headers := a.Headers
		if a.Body != "" && headers == nil {
			headers = map[string]string{"Content-Type": "application/json"}
		}
		msg.Actions = append(msg.Actions, ntfyAction{
			Action:  "http",
			Label:   a.Label,
			URL:     a.URL,
			Method:  method,
			Headers: headers,
			Body:    a.Body,
			Clear:   true,
		})
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ntfy: marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.server(d), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ntfy: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := strings.TrimSpace(d.Config["token"]); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: publish: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpError{Transport: "ntfy", Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	return nil
}
