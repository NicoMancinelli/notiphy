// Package model holds the domain types shared by the store, transports,
// router, and HTTP API. It deliberately has no dependencies on any of them.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// NewID returns a prefixed random identifier, e.g. "evt_9f3a...".
func NewID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// NewToken returns a webhook token. Hark uses the "whk_" prefix and so do we,
// so tooling written against Hark keeps working.
func NewToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "whk_" + hex.EncodeToString(b)
}

// Platform identifies the kind of device a transport delivers to.
type Platform string

const (
	PlatformIOS     Platform = "ios"
	PlatformAndroid Platform = "android"
	PlatformWeb     Platform = "web"
	PlatformOther   Platform = "other"
)

// Caps describes what a transport can actually do for a given device. The
// router reads these to decide how much of a request it must degrade.
type Caps struct {
	// Buttons: can render tappable action buttons on the notification itself,
	// so an approval resolves in one tap without opening a browser.
	Buttons bool
	// LiveActivity: can render a stateful, updating card (ActivityKit).
	LiveActivity bool
	// Images: can display ImageURL inline.
	Images bool
	// InlineReply: can collect free text from the notification.
	InlineReply bool
	// Update: can mutate a notification already on screen rather than
	// stacking a new one.
	Update bool
}

// Score ranks transports so the router can prefer the richest one available
// for a device. Buttons and LiveActivity dominate because they are the two
// capabilities that change the shape of the user's interaction.
func (c Caps) Score() int {
	s := 0
	if c.LiveActivity {
		s += 8
	}
	if c.Buttons {
		s += 4
	}
	if c.InlineReply {
		s += 2
	}
	if c.Update {
		s++
	}
	if c.Images {
		s++
	}
	return s
}

// Device is one registered delivery target.
type Device struct {
	ID        string
	AccountID string
	Name      string
	Transport string // "ntfy", "webpush", "apns"
	Platform  Platform
	Config    map[string]string // transport-specific: ntfy topic, webpush keys, apns token
	CreatedAt time.Time
	LastSeen  *time.Time
	Disabled  bool
}

// Token is a webhook credential. The token string is the sole secret, matching
// Hark's model: whoever holds the URL can publish.
type Token struct {
	ID        string
	AccountID string
	Token     string
	Name      string
	CreatedAt time.Time
	Revoked   bool
}

// ResponseType enumerates the interactive response kinds Hark supports.
type ResponseType string

const (
	ResponseApproval ResponseType = "approval"
	ResponseYesNo    ResponseType = "yes_no"
	ResponseText     ResponseType = "text"
)

// Valid reports whether t is a response type we understand.
func (t ResponseType) Valid() bool {
	switch t {
	case ResponseApproval, ResponseYesNo, ResponseText:
		return true
	}
	return false
}

// Choices returns the button labels and values for a response type. Text
// responses have no fixed choices and return nil.
func (t ResponseType) Choices() []Choice {
	switch t {
	case ResponseApproval:
		return []Choice{{Label: "Approve", Value: "approve"}, {Label: "Deny", Value: "deny"}}
	case ResponseYesNo:
		return []Choice{{Label: "Yes", Value: "yes"}, {Label: "No", Value: "no"}}
	}
	return nil
}

// Choice is one selectable answer.
type Choice struct {
	Label string
	Value string
}

// ResponseStatus tracks the lifecycle of an interactive response.
type ResponseStatus string

const (
	StatusPending   ResponseStatus = "pending"
	StatusAnswered  ResponseStatus = "answered"
	StatusExpired   ResponseStatus = "expired"
	StatusCancelled ResponseStatus = "cancelled"
)

// Callback is an optional endpoint notified when a response is answered.
type Callback struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// ResponseSpec is the caller's request for an interactive response.
type ResponseSpec struct {
	Type             ResponseType `json:"type"`
	ExpiresInSeconds int          `json:"expiresInSeconds,omitempty"`
	CorrelationID    string       `json:"correlationId,omitempty"`
	Callback         *Callback    `json:"callback,omitempty"`
}

// Response is the stored state of an interactive response.
type Response struct {
	ID            string
	EventID       string
	Type          ResponseType
	Status        ResponseStatus
	CorrelationID string
	Answer        string // "approve"/"deny"/"yes"/"no", or free text
	AnsweredBy    string // device ID or "web"
	Callback      *Callback
	// Secret authorizes the approval page without a login. It is a
	// capability URL: knowing it is what grants the right to answer.
	Secret     string
	ExpiresAt  time.Time
	AnsweredAt *time.Time
	CreatedAt  time.Time
}

// Answered reports whether the response reached a terminal state.
func (r *Response) Answered() bool { return r.Status == StatusAnswered }

// Terminal reports whether no further transitions are possible.
func (r *Response) Terminal() bool {
	return r.Status == StatusAnswered || r.Status == StatusExpired || r.Status == StatusCancelled
}

// Action is a button rendered on a notification by transports that support
// them. Transports without button support ignore these; the router will have
// already rewritten the notification to point at the approval page instead.
type Action struct {
	Label   string
	Value   string
	URL     string
	Method  string
	Headers map[string]string
	Body    string
}

// Notification is a single push, after the router has resolved degradation.
type Notification struct {
	EventID  string
	Title    string
	Body     string
	ImageURL string
	URL      string
	Priority int
	Tags     []string
	Actions  []Action
	// Response is set when this notification is asking a question.
	Response *Response
}

// Event is the stored record of one publish request.
type Event struct {
	ID        string
	AccountID string
	TokenID   string
	Title     string
	Body      string
	ImageURL  string
	URL       string
	Priority  int
	Delivered int
	CreatedAt time.Time
}

// ActivityState tracks whether a Live Activity is still running.
type ActivityState string

const (
	ActivityActive ActivityState = "active"
	ActivityEnded  ActivityState = "ended"
)

// Activity is a stateful Live Activity. The state machine is authoritative in
// SQLite regardless of whether any transport can render it natively, so
// start/update/end always succeed and /live/:id always has something to show.
type Activity struct {
	ID          string
	AccountID   string
	TokenID     string
	Key         string
	Title       string
	Status      string
	Progress    *float64
	Symbol      string
	AccentColor string
	Style       string
	State       ActivityState
	Seq         int
	// LastNotifiedProgress and LastNotifiedStatus record the state at the most
	// recent push, so the milestone throttle can tell a meaningful change from
	// a no-op on transports that stack notifications rather than update in place.
	LastNotifiedProgress float64
	LastNotifiedStatus   string
	ExpiresAt            time.Time
	StaleAt              time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	EndedAt              *time.Time
}

// ValidStyles are the layout styles Hark exposes. We accept all of them so
// payloads port over unchanged; transports that cannot render a style fall
// back to "standard".
var ValidStyles = map[string]bool{
	"standard": true, "ring": true, "hero": true, "terminal": true, "steps": true,
	"approval": true, "shell": true, "verdict": true, "signal": true,
}

// Delivery records one attempt to push to one device.
type Delivery struct {
	ID         string
	EventID    string
	ActivityID string
	DeviceID   string
	Transport  string
	OK         bool
	Error      string
	CreatedAt  time.Time
}
