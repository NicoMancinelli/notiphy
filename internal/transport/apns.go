package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/sideshow/apns2"
	apnstoken "github.com/sideshow/apns2/token"
)

// APNs delivers through Apple Push Notification service to a first-party
// notiphy iOS app.
//
// This is the only transport that can render Live Activities, the Dynamic
// Island, and native one-tap action buttons — the three things the free
// transports structurally cannot do. It stays unregistered unless the APNs
// settings are filled in, because the `aps-environment` entitlement it needs is
// available only under a paid Apple Developer Program membership. Personal/free
// Apple teams cannot obtain it at any tier of effort.
//
// Nothing else in notiphy changes when this is switched on: the router simply
// sees a transport whose Caps() outrank the others and starts preferring it.
type APNs struct {
	client *apns2.Client
	topic  string
}

// NewAPNs builds the APNs transport from a .p8 auth key.
func NewAPNs(keyFile, keyID, teamID, topic string, production bool) (*APNs, error) {
	authKey, err := apnstoken.AuthKeyFromFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("apns: read auth key %s: %w", keyFile, err)
	}
	tok := &apnstoken.Token{AuthKey: authKey, KeyID: keyID, TeamID: teamID}

	client := apns2.NewTokenClient(tok)
	if production {
		client = client.Production()
	} else {
		client = client.Development()
	}
	return &APNs{client: client, topic: topic}, nil
}

// Name identifies this transport.
func (a *APNs) Name() string { return "apns" }

// Caps reports the full capability set — this is the reference implementation
// every other transport is degrading away from.
func (a *APNs) Caps() model.Caps {
	return model.Caps{
		Buttons:      true,
		LiveActivity: true,
		Images:       true,
		InlineReply:  true,
		Update:       true,
	}
}

// Validate requires a device token from the iOS app's registration call.
func (a *APNs) Validate(d *model.Device) error {
	if strings.TrimSpace(d.Config["token"]) == "" {
		return fmt.Errorf("apns device requires a %q config value (the device token from the iOS app)", "token")
	}
	return nil
}

// ActivityTokenKey returns the device-config key holding the ActivityKit update
// token for a given activity. The iOS app posts this back after ActivityKit
// hands it over, because a Live Activity can only be updated with its own token.
func ActivityTokenKey(activityID string) string { return "act_" + activityID }

// PushToStartKey is the device-config key holding the push-to-start token,
// which lets the server begin an activity while the app is not running.
const PushToStartKey = "push_to_start"

// Send delivers a standard alert notification.
func (a *APNs) Send(ctx context.Context, d *model.Device, n *model.Notification) error {
	aps := map[string]any{
		"alert": map[string]string{
			"title": n.Title,
			"body":  n.Body,
		},
		"sound":              "default",
		"mutable-content":    1,
		"interruption-level": interruptionLevel(n.Priority),
	}
	// The category maps to a UNNotificationCategory registered in the app,
	// which is what renders the Approve/Deny buttons without opening Safari.
	if n.Response != nil {
		aps["category"] = "NOTIPHY_" + strings.ToUpper(string(n.Response.Type))
	}

	payload := map[string]any{"aps": aps}
	if n.URL != "" {
		payload["url"] = n.URL
	}
	if n.ImageURL != "" {
		payload["imageUrl"] = n.ImageURL
	}
	if n.Response != nil {
		payload["responseId"] = n.Response.ID
		payload["responseSecret"] = n.Response.Secret
		payload["responseType"] = string(n.Response.Type)
	}
	if len(n.Actions) > 0 {
		acts := make([]map[string]string, 0, len(n.Actions))
		for _, act := range n.Actions {
			acts = append(acts, map[string]string{"label": act.Label, "value": act.Value, "url": act.URL})
		}
		payload["actions"] = acts
	}

	return a.push(ctx, d.Config["token"], a.topic, apns2.PushTypeAlert, payload)
}

// StartActivity begins a Live Activity using the device's push-to-start token.
func (a *APNs) StartActivity(ctx context.Context, d *model.Device, act *model.Activity) error {
	tok := d.Config[PushToStartKey]
	if tok == "" {
		return fmt.Errorf("apns: device %s has no push-to-start token registered", d.ID)
	}
	payload := map[string]any{
		"aps": map[string]any{
			"timestamp":       time.Now().Unix(),
			"event":           "start",
			"content-state":   activityContentState(act),
			"attributes-type": "NotiphyActivityAttributes",
			"attributes": map[string]any{
				"activityId": act.ID,
				"title":      act.Title,
				"style":      act.Style,
				"accent":     act.AccentColor,
				"symbol":     act.Symbol,
			},
			"stale-date":     act.StaleAt.Unix(),
			"dismissal-date": act.ExpiresAt.Unix(),
			"alert": map[string]string{
				"title": act.Title,
				"body":  act.Status,
			},
		},
	}
	return a.push(ctx, tok, a.topic+".push-type.liveactivity", apns2.PushTypeLiveActivity, payload)
}

// UpdateActivity pushes new content state to a running Live Activity.
func (a *APNs) UpdateActivity(ctx context.Context, d *model.Device, act *model.Activity) error {
	tok := d.Config[ActivityTokenKey(act.ID)]
	if tok == "" {
		return fmt.Errorf("apns: no update token registered for activity %s", act.ID)
	}
	payload := map[string]any{
		"aps": map[string]any{
			"timestamp":     time.Now().Unix(),
			"event":         "update",
			"content-state": activityContentState(act),
			"stale-date":    act.StaleAt.Unix(),
		},
	}
	return a.push(ctx, tok, a.topic+".push-type.liveactivity", apns2.PushTypeLiveActivity, payload)
}

// EndActivity dismisses a Live Activity.
func (a *APNs) EndActivity(ctx context.Context, d *model.Device, act *model.Activity) error {
	tok := d.Config[ActivityTokenKey(act.ID)]
	if tok == "" {
		return fmt.Errorf("apns: no update token registered for activity %s", act.ID)
	}
	payload := map[string]any{
		"aps": map[string]any{
			"timestamp":      time.Now().Unix(),
			"event":          "end",
			"content-state":  activityContentState(act),
			"dismissal-date": time.Now().Add(2 * time.Minute).Unix(),
		},
	}
	return a.push(ctx, tok, a.topic+".push-type.liveactivity", apns2.PushTypeLiveActivity, payload)
}

func activityContentState(act *model.Activity) map[string]any {
	progress := 0.0
	if act.Progress != nil {
		progress = *act.Progress
	}
	return map[string]any{
		"status":    act.Status,
		"progress":  progress,
		"seq":       act.Seq,
		"updatedAt": act.UpdatedAt.Unix(),
	}
}

// interruptionLevel maps our 1-5 priority onto APNs interruption levels so a
// high-priority approval breaks through Focus.
func interruptionLevel(priority int) string {
	switch {
	case priority >= 5:
		return "critical"
	case priority == 4:
		return "time-sensitive"
	case priority <= 2:
		return "passive"
	default:
		return "active"
	}
}

func (a *APNs) push(ctx context.Context, deviceToken, topic string, pushType apns2.EPushType, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("apns: marshal payload: %w", err)
	}

	res, err := a.client.PushWithContext(ctx, &apns2.Notification{
		DeviceToken: deviceToken,
		Topic:       topic,
		PushType:    pushType,
		Payload:     body,
	})
	if err != nil {
		return fmt.Errorf("apns: push: %w", err)
	}
	if !res.Sent() {
		return &httpError{Transport: "apns", Status: res.StatusCode, Body: res.Reason}
	}
	return nil
}
