package httpx

import (
	"testing"
	"time"
)

// fakeClock drives the breaker's timers deterministically, so the state
// machine is tested rather than the scheduler.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestBreaker(threshold, probes int, openFor time.Duration) (*Breaker, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	return NewBreaker(BreakerSettings{FailureThreshold: threshold, OpenFor: openFor, HalfOpenProbes: probes, now: clock.Now}), clock
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	breaker, _ := newTestBreaker(3, 1, time.Minute)

	for i := 0; i < 2; i++ {
		if !breaker.Allow() {
			t.Fatalf("call %d must be allowed while the circuit is closed", i)
		}
		breaker.Fail(true)
		if state := breaker.State(); state != BreakerClosed {
			t.Fatalf("after %d failures state = %q, want %q", i+1, state, BreakerClosed)
		}
	}

	breaker.Allow()
	breaker.Fail(true)
	if state := breaker.State(); state != BreakerOpen {
		t.Fatalf("state = %q, want %q", state, BreakerOpen)
	}
	if breaker.Allow() {
		t.Fatal("an open circuit must not admit a call")
	}
}

// The threshold counts consecutive failures. A success in between means the
// upstream is not consistently broken.
func TestSuccessResetsTheFailureCount(t *testing.T) {
	breaker, _ := newTestBreaker(3, 1, time.Minute)
	breaker.Allow()
	breaker.Fail(true)
	breaker.Allow()
	breaker.Fail(true)
	breaker.Allow()
	breaker.Succeed()
	breaker.Allow()
	breaker.Fail(true)
	breaker.Allow()
	breaker.Fail(true)

	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q: the success reset the count", state, BreakerClosed)
	}
}

// Deterministic failures say nothing about availability, so they must never
// trip the circuit and take out calls that would have succeeded.
func TestDeterministicFailuresAreNotCounted(t *testing.T) {
	breaker, _ := newTestBreaker(2, 1, time.Minute)
	for i := 0; i < 10; i++ {
		breaker.Allow()
		breaker.Fail(false)
	}
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	breaker, clock := newTestBreaker(1, 1, time.Minute)
	breaker.Allow()
	breaker.Fail(true)
	if breaker.Allow() {
		t.Fatal("setup: the circuit should be open")
	}

	clock.Advance(time.Minute)
	if state := breaker.State(); state != BreakerHalfOpen {
		t.Fatalf("after the open period state = %q, want %q", state, BreakerHalfOpen)
	}
	if !breaker.Allow() {
		t.Fatal("half-open must admit a probe")
	}
	if breaker.Allow() {
		t.Fatal("half-open must admit only the configured number of probes")
	}

	breaker.Succeed()
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
	if !breaker.Allow() {
		t.Fatal("a closed circuit must admit calls again")
	}
}

// A failed probe means the upstream has not recovered. Serving the full open
// period again avoids probing a broken upstream in a tight loop.
func TestFailedProbeReopensForTheFullPeriod(t *testing.T) {
	breaker, clock := newTestBreaker(1, 1, time.Minute)
	breaker.Allow()
	breaker.Fail(true)

	clock.Advance(time.Minute)
	if !breaker.Allow() {
		t.Fatal("half-open must admit a probe")
	}
	breaker.Fail(true)

	if state := breaker.State(); state != BreakerOpen {
		t.Fatalf("state = %q, want %q", state, BreakerOpen)
	}
	if breaker.Allow() {
		t.Fatal("a failed probe must not leave the circuit admitting calls")
	}

	clock.Advance(59 * time.Second)
	if breaker.Allow() {
		t.Fatal("the open period must be served again in full")
	}
	clock.Advance(time.Second)
	if !breaker.Allow() {
		t.Fatal("the circuit must probe again once the period elapses")
	}
}

func TestHalfOpenRequiresEveryProbeToSucceed(t *testing.T) {
	breaker, clock := newTestBreaker(1, 2, time.Minute)
	breaker.Allow()
	breaker.Fail(true)
	clock.Advance(time.Minute)

	for probe := 1; probe <= 2; probe++ {
		if !breaker.Allow() {
			t.Fatalf("half-open must admit probe %d of the two configured", probe)
		}
	}
	if breaker.Allow() {
		t.Fatal("half-open must not admit a third probe")
	}
	breaker.Succeed()
	if state := breaker.State(); state != BreakerHalfOpen {
		t.Fatalf("state = %q, want %q after one of two probes", state, BreakerHalfOpen)
	}
	breaker.Succeed()
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

// A zero threshold disables the breaker entirely, which is how an adapter
// opts out without the call sites branching.
func TestDisabledBreakerAlwaysAllows(t *testing.T) {
	breaker := NewBreaker(BreakerSettings{FailureThreshold: 0})
	for i := 0; i < 100; i++ {
		if !breaker.Allow() {
			t.Fatal("a disabled breaker must always allow")
		}
		breaker.Fail(true)
	}
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

// A nil breaker is usable, so an adapter built without one needs no nil check
// on every call.
func TestNilBreakerAllows(t *testing.T) {
	var breaker *Breaker
	if !breaker.Allow() {
		t.Fatal("a nil breaker must allow")
	}
	breaker.Succeed()
	breaker.Fail(true)
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

func TestBreakerStats(t *testing.T) {
	breaker, _ := newTestBreaker(2, 1, time.Minute)
	breaker.Allow()
	breaker.Fail(true)

	stats := breaker.Stats()
	if stats.State != BreakerClosed || stats.ConsecutiveFails != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	breaker.Allow()
	breaker.Fail(true)
	breaker.Allow() // short-circuited
	stats = breaker.Stats()
	if stats.State != BreakerOpen {
		t.Fatalf("state = %q, want %q", stats.State, BreakerOpen)
	}
	if stats.Transitions != 1 {
		t.Fatalf("transitions = %d, want 1", stats.Transitions)
	}
	if stats.ShortCircuited != 1 {
		t.Fatalf("short-circuited = %d, want 1", stats.ShortCircuited)
	}
}

// The breaker is consulted from every request-handling goroutine at once.
func TestBreakerIsSafeUnderConcurrency(t *testing.T) {
	breaker, _ := newTestBreaker(5, 1, time.Millisecond)
	done := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		go func(worker int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				if breaker.Allow() {
					if (worker+i)%3 == 0 {
						breaker.Fail(true)
					} else {
						breaker.Succeed()
					}
				}
				_ = breaker.Stats()
			}
		}(worker)
	}
	for worker := 0; worker < 8; worker++ {
		<-done
	}
}
