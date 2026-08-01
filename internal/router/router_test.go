package router

import (
	"strings"
	"testing"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

func testRouter() *Router {
	return &Router{baseURL: "https://n.example", progressStep: 0.25}
}

// TestAdaptDegradesApprovals is the core guarantee: one payload from the
// caller, adapted per device, with the weaker client still able to answer.
func TestAdaptDegradesApprovals(t *testing.T) {
	r := testRouter()
	resp := &model.Response{
		ID:     "rsp_1",
		Type:   model.ResponseApproval,
		Secret: "s_abc",
	}
	base := model.Notification{Title: "Permission", Body: "Run Bash?"}

	tests := []struct {
		name string
		caps model.Caps
	}{
		{"buttons-capable (APNs-like)", model.Caps{Buttons: true, Images: true}},
		{"no buttons (ntfy/webpush on iOS)", model.Caps{Images: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.adapt(base, tc.caps, resp)

			// Every client must be able to reach the approval page by tapping
			// the notification body — that is the universal fallback.
			want := "https://n.example/a/s_abc"
			if got.URL != want {
				t.Errorf("tap target = %q, want %q", got.URL, want)
			}

			// Buttons are attached unconditionally: clients that render them
			// get one-tap, clients that do not simply ignore the field.
			if len(got.Actions) != 2 {
				t.Fatalf("got %d actions, want 2", len(got.Actions))
			}
			if got.Actions[0].Value != "approve" || got.Actions[1].Value != "deny" {
				t.Errorf("actions = %q/%q, want approve/deny",
					got.Actions[0].Value, got.Actions[1].Value)
			}
			if got.Actions[0].URL != "https://n.example/a/s_abc/approve" {
				t.Errorf("approve URL = %q", got.Actions[0].URL)
			}
			if got.Response == nil {
				t.Error("Response must be carried through so transports can set a category")
			}
		})
	}
}

func TestAdaptTextReplyHint(t *testing.T) {
	r := testRouter()
	resp := &model.Response{Type: model.ResponseText, Secret: "s_t"}
	base := model.Notification{Body: "What version?"}

	// A client with no inline reply needs to be told to open the page;
	// otherwise the user looks for a text field that is not there.
	got := r.adapt(base, model.Caps{}, resp)
	if !strings.Contains(got.Body, "Tap to reply") {
		t.Errorf("body = %q, want a hint to tap through", got.Body)
	}

	// A client that can reply inline should not get the hint.
	got = r.adapt(base, model.Caps{InlineReply: true}, resp)
	if strings.Contains(got.Body, "Tap to reply") {
		t.Errorf("body = %q, should not nag a client that supports inline reply", got.Body)
	}
}

func TestAdaptStripsImageWhenUnsupported(t *testing.T) {
	r := testRouter()
	base := model.Notification{Body: "hi", ImageURL: "https://img.example/a.png"}

	if got := r.adapt(base, model.Caps{Images: true}, nil); got.ImageURL == "" {
		t.Error("image dropped from a transport that supports images")
	}
	if got := r.adapt(base, model.Caps{}, nil); got.ImageURL != "" {
		t.Errorf("image = %q, want it dropped for a transport without image support", got.ImageURL)
	}
}

func TestAdaptDefaultsPriority(t *testing.T) {
	r := testRouter()
	if got := r.adapt(model.Notification{Body: "x"}, model.Caps{}, nil); got.Priority != 3 {
		t.Errorf("priority = %d, want the default 3", got.Priority)
	}
}

// TestShouldNotifyThrottle covers the milestone rule that keeps a chatty
// activity from becoming a notification flood on clients that cannot update a
// card in place.
func TestShouldNotifyThrottle(t *testing.T) {
	r := testRouter() // progressStep = 0.25

	f := func(v float64) *float64 { return &v }

	tests := []struct {
		name  string
		act   model.Activity
		phase ActivityPhase
		want  bool
	}{
		{
			name:  "start always notifies",
			act:   model.Activity{Progress: f(0)},
			phase: PhaseStart,
			want:  true,
		},
		{
			name:  "end always notifies",
			act:   model.Activity{Progress: f(1)},
			phase: PhaseEnd,
			want:  true,
		},
		{
			name:  "small progress step is suppressed",
			act:   model.Activity{Progress: f(0.2), LastNotifiedProgress: 0.1, Status: "Building", LastNotifiedStatus: "Building"},
			phase: PhaseUpdate,
			want:  false,
		},
		{
			name:  "progress past the step notifies",
			act:   model.Activity{Progress: f(0.6), LastNotifiedProgress: 0.1, Status: "Building", LastNotifiedStatus: "Building"},
			phase: PhaseUpdate,
			want:  true,
		},
		{
			name:  "status change notifies even with tiny progress",
			act:   model.Activity{Progress: f(0.12), LastNotifiedProgress: 0.1, Status: "Testing", LastNotifiedStatus: "Building"},
			phase: PhaseUpdate,
			want:  true,
		},
		{
			name:  "no progress and no status change is suppressed",
			act:   model.Activity{Status: "Building", LastNotifiedStatus: "Building"},
			phase: PhaseUpdate,
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.shouldNotify(&tc.act, tc.phase); got != tc.want {
				t.Errorf("shouldNotify = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActivityNotificationRendersProgress(t *testing.T) {
	r := testRouter()
	p := 0.6
	act := &model.Activity{ID: "act_1", Title: "Deploy", Status: "Testing", Progress: &p}

	n := r.activityNotification(act, PhaseUpdate, model.Caps{})
	if n.Body != "Testing · 60%" {
		t.Errorf("body = %q, want %q", n.Body, "Testing · 60%")
	}
	if n.URL != "https://n.example/live/act_1" {
		t.Errorf("URL = %q, want the live view", n.URL)
	}
	// A stable tag across an activity's pushes lets capable clients replace
	// the previous card rather than stacking a new one each update.
	if n.EventID != "act-act_1" {
		t.Errorf("EventID = %q, want a stable per-activity tag", n.EventID)
	}
}
