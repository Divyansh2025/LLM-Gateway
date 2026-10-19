package breaker

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestOpensAfterThresholdConsecutiveFailures(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := New(3, time.Minute, c.now)

	b.Failure()
	b.Failure()
	b.Success() // a success resets the count
	b.Failure()
	b.Failure()
	if b.State() != Closed || !b.Allow() {
		t.Fatal("should still be closed after 2 consecutive failures")
	}
	b.Failure()
	if b.State() != Open || b.Allow() {
		t.Fatal("should be open and refuse requests after 3 consecutive failures")
	}
}

func TestHalfOpenLetsOneProbeThrough(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := New(1, time.Minute, c.now)
	b.Failure()

	c.advance(59 * time.Second)
	if b.Allow() {
		t.Fatal("should refuse during cooldown")
	}
	c.advance(time.Second)
	if !b.Allow() {
		t.Fatal("should allow a probe after cooldown")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state = %v, want half-open", b.State())
	}
	if b.Allow() {
		t.Fatal("should allow only one probe at a time")
	}
}

func TestProbeResultDecidesState(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := New(5, time.Minute, c.now)
	for range 5 {
		b.Failure()
	}

	c.advance(time.Minute)
	b.Allow()
	b.Failure()
	if b.State() != Open {
		t.Fatalf("failed probe: state = %v, want open", b.State())
	}

	c.advance(time.Minute)
	b.Allow()
	b.Success()
	if b.State() != Closed || !b.Allow() {
		t.Fatalf("successful probe: state = %v, want closed", b.State())
	}
}

func TestLostProbeIsReplacedAfterCooldown(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := New(1, time.Minute, c.now)
	b.Failure()
	c.advance(time.Minute)
	b.Allow() // probe starts and never reports back

	c.advance(time.Minute)
	if !b.Allow() {
		t.Fatal("should allow a new probe once the lost one has had a full cooldown")
	}
}
