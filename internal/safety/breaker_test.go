package safety

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBreakerOpensAfterThreshold(t *testing.T) {
	b := NewBreaker("", 3, 10*time.Minute)
	sig := "cat:some failure"
	for i := 0; i < 3; i++ {
		if ok, _ := b.Allow(sig); !ok {
			t.Fatalf("iteration %d: circuit should be closed", i)
		}
		b.RecordOutcome(sig, false, "boom")
	}
	if ok, why := b.Allow(sig); ok {
		t.Fatalf("circuit should be open after 3 failures, allowed: %s", why)
	}
	// A different signature is unaffected.
	if ok, _ := b.Allow("other:signature"); !ok {
		t.Fatal("unrelated signature must not be blocked")
	}
}

func TestBreakerPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "breaker.json")
	b := NewBreaker(path, 2, 10*time.Minute)
	sig := "cat:persist"
	_, _ = b.Allow(sig)
	b.RecordOutcome(sig, false, "a")
	_, _ = b.Allow(sig)
	b.RecordOutcome(sig, false, "b")
	if ok, _ := b.Allow(sig); ok {
		t.Fatal("circuit should be open after 2 failures")
	}

	// Reload from disk: the open circuit must survive the process.
	b2 := NewBreaker(path, 2, 10*time.Minute)
	if ok, _ := b2.Allow(sig); ok {
		t.Fatal("reloaded breaker must keep the circuit open")
	}
	if got := b2.Failures(sig); got != 2 {
		t.Fatalf("persisted failure count = %d, want 2", got)
	}
}

func TestBreakerCooldownHalfOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "breaker.json")
	b := NewBreaker(path, 1, 5*time.Minute)
	sig := "cat:cooldown"
	_, _ = b.Allow(sig)
	b.RecordOutcome(sig, false, "fail")
	if ok, _ := b.Allow(sig); ok {
		t.Fatal("circuit should be open")
	}
	// Jump past the cooldown.
	now := time.Now().UTC().Add(6 * time.Minute)
	b.now = func() time.Time { return now }
	// Half-open: exactly one retry allowed.
	ok1, _ := b.Allow(sig)
	if !ok1 {
		t.Fatal("half-open retry should be allowed after cooldown")
	}
	if ok2, _ := b.Allow(sig); ok2 {
		t.Fatal("second attempt before success must be blocked")
	}
	// A successful half-open retry closes the circuit.
	b.RecordOutcome(sig, true, "fixed")
	if ok, _ := b.Allow(sig); !ok {
		t.Fatal("success must close the circuit")
	}
}

func TestBreakerSuccessResets(t *testing.T) {
	b := NewBreaker("", 2, time.Minute)
	sig := "cat:reset"
	b.RecordOutcome(sig, false, "1")
	b.RecordOutcome(sig, false, "2")
	if ok, _ := b.Allow(sig); ok {
		t.Fatal("circuit should be open")
	}
	b.RecordOutcome(sig, true, "fixed")
	if got := b.Failures(sig); got != 0 {
		t.Fatalf("success must reset failures, got %d", got)
	}
	if ok, _ := b.Allow(sig); !ok {
		t.Fatal("circuit should be closed after success")
	}
}

func TestBreakerOpenSignatures(t *testing.T) {
	b := NewBreaker("", 1, time.Hour)
	a, other := "cat:a", "cat:other"
	b.RecordOutcome(a, false, "x")
	if sigs := b.OpenSignatures(); len(sigs) != 1 || sigs[0] != a {
		t.Fatalf("open signatures = %v, want [%s]", sigs, a)
	}
	b.RecordOutcome(other, false, "y")
	b.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	// After cooldown without a retry, Allow marks half-open lazily on next
	// call; OpenSignatures still lists it until success.
	_, _ = b.Allow(other)
	if sigs := b.OpenSignatures(); len(sigs) != 2 {
		t.Fatalf("both signatures should be open/half-open, got %v", sigs)
	}
}
