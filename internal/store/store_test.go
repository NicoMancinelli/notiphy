package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIdempotency(t *testing.T) {
	st := testStore(t)
	tok, err := st.CreateToken("test")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	const key = "k1"
	hashA := HashPayload([]byte(`{"body":"hello"}`))
	hashB := HashPayload([]byte(`{"body":"different"}`))

	// A key that has never been used must report not-found so the caller
	// proceeds with the real request.
	if _, err := st.LookupIdempotency(tok.ID, key, hashA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first lookup: got %v, want ErrNotFound", err)
	}

	if err := st.SaveIdempotency(tok.ID, key, hashA, 200, `{"ok":true,"eventId":"evt_1"}`); err != nil {
		t.Fatalf("save: %v", err)
	}

	// An identical replay returns the original result.
	rec, err := st.LookupIdempotency(tok.ID, key, hashA)
	if err != nil {
		t.Fatalf("replay lookup: %v", err)
	}
	if rec.StatusCode != 200 || rec.Response != `{"ok":true,"eventId":"evt_1"}` {
		t.Fatalf("replay returned %d %q, want the stored result", rec.StatusCode, rec.Response)
	}

	// The same key with a different payload is a caller bug, not a replay.
	if _, err := st.LookupIdempotency(tok.ID, key, hashB); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting lookup: got %v, want ErrIdempotencyConflict", err)
	}

	// Keys are scoped per token, so a different token may reuse the key.
	other, err := st.CreateToken("other")
	if err != nil {
		t.Fatalf("create second token: %v", err)
	}
	if _, err := st.LookupIdempotency(other.ID, key, hashA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-token lookup: got %v, want ErrNotFound", err)
	}
}

func TestRevokedTokenIsInvisible(t *testing.T) {
	st := testStore(t)
	tok, err := st.CreateToken("test")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if _, err := st.TokenByValue(tok.Token); err != nil {
		t.Fatalf("lookup before revoke: %v", err)
	}
	if err := st.RevokeToken(tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// A revoked token must be indistinguishable from one that never existed.
	if _, err := st.TokenByValue(tok.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup after revoke: got %v, want ErrNotFound", err)
	}
}

// newResponse creates an event plus a pending response expiring after ttl.
func newResponse(t *testing.T, st *Store, ttl time.Duration) *model.Response {
	t.Helper()
	tok, err := st.CreateToken("test")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	ev := &model.Event{AccountID: tok.AccountID, TokenID: tok.ID, Body: "question"}
	if err := st.CreateEvent(ev); err != nil {
		t.Fatalf("create event: %v", err)
	}
	r := &model.Response{
		EventID:   ev.ID,
		Type:      model.ResponseApproval,
		ExpiresAt: time.Now().Add(ttl),
	}
	if err := st.CreateResponse(r); err != nil {
		t.Fatalf("create response: %v", err)
	}
	return r
}

func TestAnswerResponseOnlyOnce(t *testing.T) {
	st := testStore(t)
	r := newResponse(t, st, time.Minute)

	got, err := st.AnswerResponse(r.ID, "approve", "notification")
	if err != nil {
		t.Fatalf("first answer: %v", err)
	}
	if got.Status != model.StatusAnswered || got.Answer != "approve" {
		t.Fatalf("got status=%s answer=%q, want answered/approve", got.Status, got.Answer)
	}

	// The same notification can land on several devices; the first answer wins
	// and later taps must not overwrite it.
	if _, err := st.AnswerResponse(r.ID, "deny", "web"); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("second answer: got %v, want ErrAlreadyAnswered", err)
	}

	after, err := st.ResponseBySecret(r.Secret)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Answer != "approve" {
		t.Fatalf("answer changed to %q; the first answer must win", after.Answer)
	}
}

func TestExpiredResponseCannotBeAnswered(t *testing.T) {
	st := testStore(t)
	r := newResponse(t, st, -time.Second) // already past its deadline

	// Reading an overdue response must settle it, so a poller never sees a
	// stale "pending" even if the sweeper has not run.
	got, err := st.ResponseBySecret(r.Secret)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Status != model.StatusExpired {
		t.Fatalf("got status %s, want expired", got.Status)
	}
	if _, err := st.AnswerResponse(r.ID, "approve", "web"); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("answering an expired response: got %v, want ErrAlreadyAnswered", err)
	}
}

func TestActivityKeyConflictAndReplace(t *testing.T) {
	st := testStore(t)
	now := time.Now()

	mk := func() *model.Activity {
		return &model.Activity{
			Key:       "deploy",
			Title:     "Deploy",
			ExpiresAt: now.Add(time.Hour),
			StaleAt:   now.Add(30 * time.Minute),
		}
	}

	first := mk()
	if err := st.CreateActivity(first, false); err != nil {
		t.Fatalf("create first: %v", err)
	}

	// Without replace, a second activity on the same key is a conflict.
	if err := st.CreateActivity(mk(), false); !errors.Is(err, ErrActivityConflict) {
		t.Fatalf("duplicate key: got %v, want ErrActivityConflict", err)
	}

	// With replace, the old one is ended and the new one takes the slot.
	second := mk()
	if err := st.CreateActivity(second, true); err != nil {
		t.Fatalf("create with replace: %v", err)
	}

	old, err := st.Activity(first.ID)
	if err != nil {
		t.Fatalf("reload first: %v", err)
	}
	if old.State != model.ActivityEnded {
		t.Fatalf("replaced activity is %s, want ended", old.State)
	}

	active, err := st.ActiveActivityByKey("deploy")
	if err != nil {
		t.Fatalf("lookup active: %v", err)
	}
	if active.ID != second.ID {
		t.Fatalf("active activity is %s, want the replacement %s", active.ID, second.ID)
	}
}

func TestUpdateActivityMerges(t *testing.T) {
	st := testStore(t)
	p := 0.1
	act := &model.Activity{
		Title:     "Deploy",
		Status:    "Building",
		Progress:  &p,
		ExpiresAt: time.Now().Add(time.Hour),
		StaleAt:   time.Now().Add(time.Hour),
	}
	if err := st.CreateActivity(act, false); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Patching only progress must leave title and status untouched.
	np := 0.6
	got, err := st.UpdateActivity(act.ID, ActivityPatch{Progress: &np})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Title != "Deploy" || got.Status != "Building" {
		t.Fatalf("merge patch clobbered fields: title=%q status=%q", got.Title, got.Status)
	}
	if got.Progress == nil || *got.Progress != 0.6 {
		t.Fatalf("progress not applied: %v", got.Progress)
	}
	if got.Seq != 1 {
		t.Fatalf("seq = %d, want 1", got.Seq)
	}

	// An ended activity rejects further updates.
	if err := st.EndActivity(act.ID, "Shipped", nil); err != nil {
		t.Fatalf("end: %v", err)
	}
	if _, err := st.UpdateActivity(act.ID, ActivityPatch{Status: strPtr("Late")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update after end: got %v, want ErrNotFound", err)
	}
}

func strPtr(s string) *string { return &s }
