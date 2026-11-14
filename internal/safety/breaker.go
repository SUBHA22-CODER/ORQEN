package safety

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/safefs"
)

// Breaker default thresholds: open the circuit after this many consecutive
// failed repair attempts for the same failure signature, and keep it open for
// this long before allowing a single half-open retry.
const (
	DefaultFailureThreshold = 3
	DefaultCooldown         = 10 * time.Minute
)

type openState struct {
	OpenedAt time.Time `json:"opened_at"`
	Until    time.Time `json:"until"`
	Retried  bool      `json:"retried"`
}

type breakerState struct {
	Failures map[string]int            `json:"failures"`
	Open     map[string]openState      `json:"open,omitempty"`
	History  map[string][]breakerEvent `json:"history,omitempty"`
}

type breakerEvent struct {
	At      time.Time `json:"at"`
	Success bool      `json:"success"`
	Detail  string    `json:"detail,omitempty"`
}

// Breaker prevents repeated unsuccessful repair attempts. State is persisted
// per project so autonomous healing stays paused across CLI invocations until
// the cooldown elapses or a repair succeeds.
type Breaker struct {
	mu        sync.Mutex
	path      string
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	maxHist   int
	loaded    *breakerState
}

// NewBreaker loads (or creates) a breaker persisted at path. Empty path means
// memory-only. Threshold <= 0 selects the default; cooldown <= 0 selects the
// default.
func NewBreaker(path string, threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = DefaultFailureThreshold
	}
	if cooldown <= 0 {
		cooldown = DefaultCooldown
	}
	b := &Breaker{
		path:      path,
		threshold: threshold,
		cooldown:  cooldown,
		now:       func() time.Time { return time.Now().UTC() },
		maxHist:   20,
		loaded: &breakerState{
			Failures: map[string]int{},
			Open:     map[string]openState{},
			History:  map[string][]breakerEvent{},
		},
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var st breakerState
			if json.Unmarshal(data, &st) == nil {
				if st.Failures != nil {
					b.loaded.Failures = st.Failures
				}
				if st.Open != nil {
					b.loaded.Open = st.Open
				}
				if st.History != nil {
					b.loaded.History = st.History
				}
			}
		}
	}
	return b
}

// Allow reports whether autonomous repair may proceed for the signature. A
// freshly opened circuit blocks; after the cooldown exactly one half-open
// retry is allowed; a second attempt before a success blocks again.
func (b *Breaker) Allow(signature string) (bool, string) {
	if signature == "" {
		return true, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.loaded
	if os, open := st.Open[signature]; open {
		now := b.now()
		if now.Before(os.Until) {
			return false, fmt.Sprintf("circuit open for %s since %s (retry allowed after %s)",
				signature, os.OpenedAt.Format(time.RFC3339), os.Until.Format(time.RFC3339))
		}
		if os.Retried {
			return false, fmt.Sprintf("circuit open for %s: half-open retry already attempted without success", signature)
		}
		os.Retried = true
		st.Open[signature] = os
		_ = b.save()
		return true, "half-open retry allowed for " + signature
	}
	if n := st.Failures[signature]; n >= b.threshold {
		until := b.now().Add(b.cooldown)
		st.Open[signature] = openState{OpenedAt: b.now(), Until: until}
		_ = b.save()
		return false, fmt.Sprintf("circuit opened for %s after %d consecutive failed repairs (cooldown until %s)",
			signature, n, until.Format(time.RFC3339))
	}
	return true, ""
}

// RecordOutcome records a repair outcome for the signature. Success resets
// the consecutive-failure count and closes an open circuit. Failure
// increments the count; the next Allow call opens the circuit once the
// threshold is reached.
func (b *Breaker) RecordOutcome(signature string, success bool, detail string) {
	if signature == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.loaded
	if success {
		delete(st.Failures, signature)
		delete(st.Open, signature)
	} else {
		st.Failures[signature]++
		delete(st.Open, signature) // failed half-open attempt: reopen lazily on next Allow
	}
	hist := st.History[signature]
	hist = append(hist, breakerEvent{At: b.now(), Success: success, Detail: detail})
	if len(hist) > b.maxHist {
		hist = hist[len(hist)-b.maxHist:]
	}
	st.History[signature] = hist
	_ = b.save()
}

// Failures reports the consecutive failure count for a signature.
func (b *Breaker) Failures(signature string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loaded.Failures[signature]
}

// OpenSignatures lists signatures that are blocked or will be blocked on the
// next Allow call: open circuits plus failure counts at or above the
// threshold. Used for status reporting.
func (b *Breaker) OpenSignatures() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	now := b.now()
	for sig, os := range b.loaded.Open {
		if now.Before(os.Until) || os.Retried {
			if !seen[sig] {
				seen[sig] = true
				out = append(out, sig)
			}
		}
	}
	for sig, n := range b.loaded.Failures {
		if n >= b.threshold && !seen[sig] {
			seen[sig] = true
			out = append(out, sig)
		}
	}
	return out
}

// save persists state atomically; memory-only breakers skip I/O.
func (b *Breaker) save() error {
	if b.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(b.loaded, "", "  ")
	if err != nil {
		return err
	}
	return safefs.WriteFileAtomic(b.path, append(data, '\n'), 0o600)
}
