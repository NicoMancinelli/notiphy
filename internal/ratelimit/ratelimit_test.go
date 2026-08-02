package ratelimit

import (
	"testing"
	"time"
)

func TestDisabledByDefault(t *testing.T) {
	l := New(0, time.Minute)
	if l.Enabled() {
		t.Fatal("a zero limit must disable limiting; this is a self-hosted server")
	}
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d denied by a disabled limiter", i)
		}
	}
}

func TestAllowsUpToLimitThenDenies(t *testing.T) {
	l := New(3, time.Minute)

	for i := 1; i <= 3; i++ {
		if ok, _ := l.Allow("token-a"); !ok {
			t.Fatalf("request %d should have been allowed", i)
		}
	}

	ok, retry := l.Allow("token-a")
	if ok {
		t.Fatal("the 4th request should have been denied")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry-after = %v, want something inside the window", retry)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, time.Minute)

	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("first request for a denied")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("second request for a should be denied")
	}
	// A different token must not be affected by another's exhaustion.
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("request for b denied because a was exhausted")
	}
}

func TestWindowResets(t *testing.T) {
	l := New(1, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first request denied")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("second request in the same window should be denied")
	}

	now = now.Add(61 * time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("request after the window closed should be allowed again")
	}
}

func TestCleanupDropsClosedWindows(t *testing.T) {
	l := New(5, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	l.Allow("a")
	l.Allow("b")
	if len(l.buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(l.buckets))
	}

	now = now.Add(2 * time.Minute)
	l.Cleanup()
	if len(l.buckets) != 0 {
		t.Fatalf("got %d buckets after cleanup, want 0", len(l.buckets))
	}
}
