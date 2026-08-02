package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/activity"
	"github.com/NicoMancinelli/notiphy/internal/callback"
	"github.com/NicoMancinelli/notiphy/internal/config"
	"github.com/NicoMancinelli/notiphy/internal/model"
	"github.com/NicoMancinelli/notiphy/internal/router"
	"github.com/NicoMancinelli/notiphy/internal/store"
	"github.com/NicoMancinelli/notiphy/internal/transport"
)

// fakeTransport records what it was asked to send so tests can assert on the
// adapted notification rather than reaching for a real push service.
type fakeTransport struct {
	mu   sync.Mutex
	sent []model.Notification
	caps model.Caps
	err  error
}

func (f *fakeTransport) Name() string                 { return "fake" }
func (f *fakeTransport) Caps() model.Caps             { return f.caps }
func (f *fakeTransport) Validate(*model.Device) error { return nil }

func (f *fakeTransport) Send(_ context.Context, _ *model.Device, n *model.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, *n)
	return nil
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeTransport) last() model.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return model.Notification{}
	}
	return f.sent[len(f.sent)-1]
}

type harness struct {
	srv   *httptest.Server
	store *store.Store
	fake  *fakeTransport
	token string
}

func newHarness(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.BaseURL = "http://example.test"
	if tweak != nil {
		tweak(&cfg)
	}

	fake := &fakeTransport{caps: model.Caps{Images: true}}
	reg := transport.NewRegistry()
	reg.Register(fake)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := activity.NewHub()
	rt := router.New(st, reg, hub, cfg.BaseURL, cfg.ActivityProgressStep, log)
	cb := callback.New(st, "test", log)

	s, err := New(cfg, st, reg, rt, hub, cb, "test-vapid-key", log)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// One device, so delivery has somewhere to go.
	if err := st.CreateDevice(&model.Device{
		Name: "test", Transport: "fake", Platform: model.PlatformIOS,
		Config: map[string]string{},
	}); err != nil {
		t.Fatalf("create device: %v", err)
	}

	tok, err := st.CreateToken("test")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	return &harness{srv: ts, store: st, fake: fake, token: tok.Token}
}

func (h *harness) post(t *testing.T, path, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return h.do(t, req)
}

func (h *harness) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return h.do(t, req)
}

func (h *harness) do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestNotifyRequiresBody(t *testing.T) {
	h := newHarness(t, nil)
	status, body := h.post(t, "/hooks/"+h.token, `{"title":"no body"}`, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(body["error"].(string), "body") {
		t.Errorf("error = %q, should name the missing field", body["error"])
	}
}

func TestUnknownTokenIs404(t *testing.T) {
	h := newHarness(t, nil)
	status, _ := h.post(t, "/hooks/whk_nope", `{"body":"x"}`, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestNotifyDelivers(t *testing.T) {
	h := newHarness(t, nil)
	status, body := h.post(t, "/hooks/"+h.token, `{"title":"Deploy","body":"done"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", status, body)
	}
	if body["delivered"].(float64) != 1 {
		t.Errorf("delivered = %v, want 1", body["delivered"])
	}
	if got := h.fake.last(); got.Title != "Deploy" || got.Body != "done" {
		t.Errorf("transport got %+v, want the submitted title/body", got)
	}
}

func TestIdempotentReplayDoesNotResend(t *testing.T) {
	h := newHarness(t, nil)
	hdr := map[string]string{"Idempotency-Key": "k1"}

	status, first := h.post(t, "/hooks/"+h.token, `{"body":"once"}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("first status = %d, want 200", status)
	}
	if h.fake.count() != 1 {
		t.Fatalf("sent %d pushes, want 1", h.fake.count())
	}

	status, replay := h.post(t, "/hooks/"+h.token, `{"body":"once"}`, hdr)
	if status != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", status)
	}
	if replay["idempotent"] != true {
		t.Error("replay should be flagged idempotent")
	}
	if replay["eventId"] != first["eventId"] {
		t.Errorf("replay eventId = %v, want the original %v", replay["eventId"], first["eventId"])
	}
	// The point of idempotency is that the phone does not buzz twice.
	if h.fake.count() != 1 {
		t.Errorf("sent %d pushes after replay, want still 1", h.fake.count())
	}

	status, _ = h.post(t, "/hooks/"+h.token, `{"body":"DIFFERENT"}`, hdr)
	if status != http.StatusConflict {
		t.Errorf("conflicting replay status = %d, want 409", status)
	}
}

func TestApprovalRoundTrip(t *testing.T) {
	h := newHarness(t, nil)

	status, body := h.post(t, "/hooks/"+h.token,
		`{"body":"Deploy?","response":{"type":"approval","expiresInSeconds":300}}`, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", status, body)
	}

	resp := body["response"].(map[string]any)
	if resp["status"] != "pending" {
		t.Fatalf("status = %v, want pending", resp["status"])
	}
	approvalURL := resp["approvalUrl"].(string)
	secret := approvalURL[strings.LastIndex(approvalURL, "/")+1:]

	// A transport without buttons must still be able to reach the page.
	if got := h.fake.last(); !strings.HasSuffix(got.URL, "/a/"+secret) {
		t.Errorf("tap target = %q, want the approval page", got.URL)
	}

	status, _ = h.post(t, "/a/"+secret+"/approve", "", nil)
	if status != http.StatusOK {
		t.Fatalf("answer status = %d, want 200", status)
	}

	eventID := body["eventId"].(string)
	status, polled := h.get(t, "/hooks/"+h.token+"/events/"+eventID)
	if status != http.StatusOK {
		t.Fatalf("poll status = %d, want 200", status)
	}
	pr := polled["response"].(map[string]any)
	if pr["status"] != "answered" || pr["answer"] != "approve" {
		t.Errorf("after answering: status=%v answer=%v", pr["status"], pr["answer"])
	}

	// A second answer must not overwrite the first.
	status, _ = h.post(t, "/a/"+secret+"/deny", "", nil)
	if status != http.StatusConflict {
		t.Errorf("double answer status = %d, want 409", status)
	}
}

func TestApprovalWithNoDevicesLeavesNoOrphan(t *testing.T) {
	h := newHarness(t, nil)

	devices, err := h.store.ListDevices(false)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	for _, d := range devices {
		if err := h.store.DeleteDevice(d.ID); err != nil {
			t.Fatalf("delete device: %v", err)
		}
	}

	status, _ := h.post(t, "/hooks/"+h.token,
		`{"body":"Deploy?","response":{"type":"approval"}}`, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when there is nowhere to deliver", status)
	}

	// The failure must not leave a pending response nobody can ever answer:
	// the caller got an error and never learned the secret.
	n, err := h.store.CountPendingResponses()
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 0 {
		t.Errorf("left %d orphaned pending response(s), want 0", n)
	}
}

func TestRateLimitReturns429(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.RateLimitPerMinute = 2 })

	for i := 1; i <= 2; i++ {
		if status, _ := h.post(t, "/hooks/"+h.token, `{"body":"x"}`, nil); status != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, status)
		}
	}

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/hooks/"+h.token, strings.NewReader(`{"body":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After so a client knows when to come back")
	}
}

func TestRateLimitOffByDefault(t *testing.T) {
	h := newHarness(t, nil)
	for i := 0; i < 25; i++ {
		if status, _ := h.post(t, "/hooks/"+h.token, `{"body":"x"}`, nil); status != http.StatusOK {
			t.Fatalf("request %d was limited; this is a self-hosted server", i)
		}
	}
}

func TestActivityLifecycle(t *testing.T) {
	h := newHarness(t, nil)

	status, body := h.post(t, "/hooks/"+h.token+"/live-activities",
		`{"title":"Deploy","status":"Building","progress":0.1,"key":"d","replace":true}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (%v)", status, body)
	}
	id := body["id"].(string)

	// No registered transport renders a native Live Activity here, so the
	// caller must be told rather than silently getting notifications.
	if body["native"] != false {
		t.Error("native should be false with only the fake transport")
	}
	if body["warning"] == nil {
		t.Error("a degraded activity should carry a warning")
	}

	req, _ := http.NewRequest(http.MethodPatch,
		h.srv.URL+"/hooks/"+h.token+"/live-activities/"+id,
		strings.NewReader(`{"status":"Testing","progress":0.6}`))
	req.Header.Set("Content-Type", "application/json")
	status, patched := h.do(t, req)
	if status != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", status)
	}
	if patched["status"] != "Testing" {
		t.Errorf("status = %v, want Testing", patched["status"])
	}
	// A merge patch must not clear the title.
	if patched["title"] != "Deploy" {
		t.Errorf("title = %v, merge patch should have left it alone", patched["title"])
	}

	status, _ = h.post(t, "/hooks/"+h.token+"/live-activities/"+id+"/end", `{"status":"Shipped"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("end status = %d, want 200", status)
	}
	status, _ = h.post(t, "/hooks/"+h.token+"/live-activities/"+id+"/end", `{}`, nil)
	if status != http.StatusConflict {
		t.Errorf("second end status = %d, want 409", status)
	}
}

func TestAdminTokenGatesOperatorSurfaceOnly(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.AdminToken = "sekret" })

	// Operator surface is closed...
	for _, path := range []string{"/", "/api/devices", "/subscribe"} {
		status, _ := h.get(t, path)
		if status != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, status)
		}
	}

	// ...but capability URLs keep working, or CI and notifications break.
	if status, _ := h.post(t, "/hooks/"+h.token, `{"body":"x"}`, nil); status != http.StatusOK {
		t.Errorf("webhook = %d, want 200; it carries its own credential", status)
	}
	if status, _ := h.get(t, "/healthz"); status != http.StatusOK {
		t.Errorf("healthz = %d, want 200", status)
	}

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/devices", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	if status, _ := h.do(t, req); status != http.StatusOK {
		t.Errorf("authorized /api/devices = %d, want 200", status)
	}
}

func TestManifestCarriesAppTokenWhenAdminSet(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.AdminToken = "sekret" })

	// Unauthenticated: no token to hand out.
	status, body := h.get(t, "/manifest.webmanifest")
	if status != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200", status)
	}
	if start, _ := body["start_url"].(string); strings.Contains(start, "t=") {
		t.Error("an unauthenticated manifest fetch must not mint an app token")
	}

	// Authenticated: start_url carries a token, because the installed PWA gets
	// its own cookie jar and this is the only channel into it.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/manifest.webmanifest", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	status, body = h.do(t, req)
	if status != http.StatusOK {
		t.Fatalf("authed manifest status = %d, want 200", status)
	}
	start, _ := body["start_url"].(string)
	if !strings.HasPrefix(start, "/app?t=app_") {
		t.Fatalf("start_url = %q, want /app with a minted token", start)
	}

	// That token must actually open the app.
	token := start[strings.Index(start, "t=")+2:]
	req, _ = http.NewRequest(http.MethodGet, h.srv.URL+"/api/app/state", nil)
	req.AddCookie(&http.Cookie{Name: appCookie, Value: token})
	if status, _ := h.do(t, req); status != http.StatusOK {
		t.Errorf("app state with minted token = %d, want 200", status)
	}
}

func TestAppStateListsPendingApprovals(t *testing.T) {
	h := newHarness(t, nil)

	h.post(t, "/hooks/"+h.token, `{"title":"Ask","body":"Deploy?","response":{"type":"approval"}}`, nil)

	status, body := h.get(t, "/api/app/state")
	if status != http.StatusOK {
		t.Fatalf("state status = %d, want 200", status)
	}
	if body["pendingCount"].(float64) != 1 {
		t.Fatalf("pendingCount = %v, want 1", body["pendingCount"])
	}
	pending := body["pending"].([]any)
	first := pending[0].(map[string]any)
	if first["title"] != "Ask" {
		t.Errorf("title = %v, want the event title", first["title"])
	}
	if first["secret"] == "" {
		t.Error("the shell needs the secret to answer in one tap")
	}
}

func TestExpiredResponseCannotBeAnswered(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DefaultResponseTTL = time.Millisecond })

	_, body := h.post(t, "/hooks/"+h.token, `{"body":"Deploy?","response":{"type":"approval"}}`, nil)
	resp := body["response"].(map[string]any)
	approvalURL := resp["approvalUrl"].(string)
	secret := approvalURL[strings.LastIndex(approvalURL, "/")+1:]

	time.Sleep(20 * time.Millisecond)

	status, _ := h.post(t, "/a/"+secret+"/approve", "", nil)
	if status != http.StatusConflict {
		t.Errorf("answering an expired response = %d, want 409", status)
	}
}
