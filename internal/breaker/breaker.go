// Package breaker implements a per-provider circuit breaker.
//
// Closed: requests flow. After Threshold consecutive failures the breaker opens.
// Open: requests are refused until Cooldown has passed, so a dead provider
// isn't hammered and callers fail over immediately instead of waiting on it.
// Half-open: one probe request is let through. Success closes the breaker,
// failure opens it again for another cooldown.
package breaker

import (
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	return [...]string{"closed", "open", "half-open"}[s]
}

type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu         sync.Mutex
	state      State
	failures   int
	openedAt   time.Time
	probeStart time.Time // zero when no half-open probe is in flight
}

// New returns a closed breaker. now is injected so tests can control time.
func New(threshold int, cooldown time.Duration, now func() time.Time) *Breaker {
	return &Breaker{threshold: threshold, cooldown: cooldown, now: now}
}

// Allow reports whether a request may be sent now.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	switch b.state {
	case Open:
		if now.Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = HalfOpen
		b.probeStart = now
		return true
	case HalfOpen:
		// Only one probe at a time. If a probe never reports back (its client
		// disconnected), allow a new one after another cooldown.
		if !b.probeStart.IsZero() && now.Sub(b.probeStart) < b.cooldown {
			return false
		}
		b.probeStart = now
		return true
	default:
		return true
	}
}

// Success records a healthy response and closes the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = Closed
	b.failures = 0
	b.probeStart = time.Time{}
}

// Failure records a failed request and opens the breaker when the threshold is
// reached, or immediately if the failed request was the half-open probe.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.state == HalfOpen || b.failures >= b.threshold {
		b.state = Open
		b.openedAt = b.now()
		b.probeStart = time.Time{}
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
