package httpx

import (
	"sync"
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

// A real caller admits a call and then reports that call's outcome, so the
// tests do both together. Anything else — a result reported against a ticket
// from an earlier episode — is a straggler, and has to be written out
// deliberately, which is the point.
func admits(b *Breaker) bool { ok, _ := b.Allow(); return ok }

func failOnce(b *Breaker, retryable bool) {
	_, ticket := b.Allow()
	b.Fail(ticket, retryable)
}

func succeedOnce(b *Breaker) {
	_, ticket := b.Allow()
	b.Succeed(ticket)
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	breaker, _ := newTestBreaker(3, 1, time.Minute)

	for i := 0; i < 2; i++ {
		if !admits(breaker) {
			t.Fatalf("call %d must be allowed while the circuit is closed", i)
		}
		failOnce(breaker, true)
		if state := breaker.State(); state != BreakerClosed {
			t.Fatalf("after %d failures state = %q, want %q", i+1, state, BreakerClosed)
		}
	}

	failOnce(breaker, true)
	if state := breaker.State(); state != BreakerOpen {
		t.Fatalf("state = %q, want %q", state, BreakerOpen)
	}
	if admits(breaker) {
		t.Fatal("an open circuit must not admit a call")
	}
}

// The threshold counts consecutive failures. A success in between means the
// upstream is not consistently broken.
func TestSuccessResetsTheFailureCount(t *testing.T) {
	breaker, _ := newTestBreaker(3, 1, time.Minute)
	failOnce(breaker, true)
	failOnce(breaker, true)
	succeedOnce(breaker)
	failOnce(breaker, true)
	failOnce(breaker, true)

	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q: the success reset the count", state, BreakerClosed)
	}
}

// Deterministic failures say nothing about availability, so they must never
// trip the circuit and take out calls that would have succeeded.
func TestDeterministicFailuresAreNotCounted(t *testing.T) {
	breaker, _ := newTestBreaker(2, 1, time.Minute)
	for i := 0; i < 10; i++ {
		failOnce(breaker, false)
	}
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	breaker, clock := newTestBreaker(1, 1, time.Minute)
	failOnce(breaker, true)
	if admits(breaker) {
		t.Fatal("setup: the circuit should be open")
	}

	clock.Advance(time.Minute)
	if state := breaker.State(); state != BreakerHalfOpen {
		t.Fatalf("after the open period state = %q, want %q", state, BreakerHalfOpen)
	}
	admitted, probe := breaker.Allow()
	if !admitted {
		t.Fatal("half-open must admit a probe")
	}
	if admits(breaker) {
		t.Fatal("half-open must admit only the configured number of probes")
	}

	// The probe's own result, reported against the ticket it was admitted
	// with. This is what closes the circuit; nothing else may.
	breaker.Succeed(probe)
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
	if !admits(breaker) {
		t.Fatal("a closed circuit must admit calls again")
	}
}

// A failed probe means the upstream has not recovered. Serving the full open
// period again avoids probing a broken upstream in a tight loop.
func TestFailedProbeReopensForTheFullPeriod(t *testing.T) {
	breaker, clock := newTestBreaker(1, 1, time.Minute)
	failOnce(breaker, true)

	clock.Advance(time.Minute)
	admitted, probe := breaker.Allow()
	if !admitted {
		t.Fatal("half-open must admit a probe")
	}
	breaker.Fail(probe, true)

	if state := breaker.State(); state != BreakerOpen {
		t.Fatalf("state = %q, want %q", state, BreakerOpen)
	}
	if admits(breaker) {
		t.Fatal("a failed probe must not leave the circuit admitting calls")
	}

	clock.Advance(59 * time.Second)
	if admits(breaker) {
		t.Fatal("the open period must be served again in full")
	}
	clock.Advance(time.Second)
	if !admits(breaker) {
		t.Fatal("the circuit must probe again once the period elapses")
	}
}

func TestHalfOpenRequiresEveryProbeToSucceed(t *testing.T) {
	breaker, clock := newTestBreaker(1, 2, time.Minute)
	failOnce(breaker, true)
	clock.Advance(time.Minute)

	tickets := make([]Ticket, 0, 2)
	for probe := 1; probe <= 2; probe++ {
		admitted, ticket := breaker.Allow()
		if !admitted {
			t.Fatalf("half-open must admit probe %d of the two configured", probe)
		}
		tickets = append(tickets, ticket)
	}
	if admits(breaker) {
		t.Fatal("half-open must not admit a third probe")
	}
	breaker.Succeed(tickets[0])
	if state := breaker.State(); state != BreakerHalfOpen {
		t.Fatalf("state = %q, want %q after one of two probes", state, BreakerHalfOpen)
	}
	breaker.Succeed(tickets[1])
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

// A zero threshold disables the breaker entirely, which is how an adapter
// opts out without the call sites branching.
func TestDisabledBreakerAlwaysAllows(t *testing.T) {
	breaker := NewBreaker(BreakerSettings{FailureThreshold: 0})
	for i := 0; i < 100; i++ {
		if !admits(breaker) {
			t.Fatal("a disabled breaker must always allow")
		}
		failOnce(breaker, true)
	}
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

// A nil breaker is usable, so an adapter built without one needs no nil check
// on every call.
func TestNilBreakerAllows(t *testing.T) {
	var breaker *Breaker
	if !admits(breaker) {
		t.Fatal("a nil breaker must allow")
	}
	breaker.Succeed(0)
	breaker.Fail(0, true)
	if state := breaker.State(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}

func TestBreakerStats(t *testing.T) {
	breaker, _ := newTestBreaker(2, 1, time.Minute)
	failOnce(breaker, true)

	stats := breaker.Stats()
	if stats.State != BreakerClosed || stats.ConsecutiveFails != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	failOnce(breaker, true)
	breaker.Allow() //nolint:errcheck // short-circuited on purpose
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
				admitted, ticket := breaker.Allow()
				if admitted {
					if (worker+i)%3 == 0 {
						breaker.Fail(ticket, true)
					} else {
						breaker.Succeed(ticket)
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

// A result from a call admitted before the circuit opened must not decide
// anything about the upstream's health now.
//
// This is the failure the ticket exists for, and it is only reachable
// concurrently: a call in flight when the circuit opens reports its outcome
// into whatever state the breaker has reached by the time it returns. The
// sequence below is what a fan-out against an upstream that goes down does on
// an ordinary day.
//
// Measured before the ticket, with the same steps: the straggler's success
// closed the circuit, and — having reset the failure count on its way — left
// the real probe's failure one short of reopening it. The upstream was down,
// the probe had just proved it, and the breaker was passing full traffic
// through.
func TestAStragglerFromBeforeTheOutageDecidesNothing(t *testing.T) {
	breaker, clock := newTestBreaker(2, 1, time.Minute)

	// A call is admitted while everything is fine. It is now in flight.
	admitted, straggler := breaker.Allow()
	if !admitted {
		t.Fatal("setup: a closed circuit must admit")
	}

	// While it is out there, the upstream fails and the circuit opens.
	failOnce(breaker, true)
	failOnce(breaker, true)
	if state := breaker.State(); state != BreakerOpen {
		t.Fatalf("setup: state = %q, want %q", state, BreakerOpen)
	}

	// The open period passes and a probe is admitted.
	clock.Advance(time.Minute)
	probeAdmitted, probe := breaker.Allow()
	if !probeAdmitted {
		t.Fatal("setup: half-open must admit a probe")
	}

	// Only now does the straggler return, successfully. It started before the
	// upstream was suspected and says nothing about it.
	breaker.Succeed(straggler)
	if state := breaker.State(); state != BreakerHalfOpen {
		t.Fatalf("state = %q, want %q: a success from a previous episode must not close the circuit", state, BreakerHalfOpen)
	}

	// The probe then fails, which is the only evidence that counts.
	breaker.Fail(probe, true)
	if state := breaker.State(); state != BreakerOpen {
		t.Fatalf("state = %q, want %q: the probe failed, so the upstream has not recovered", state, BreakerOpen)
	}
	if admits(breaker) {
		t.Fatal("the circuit must not admit calls after its probe failed")
	}
}

// A straggler's failure must not push the open window forward either, or a
// burst of in-flight calls keeps the circuit open for OpenFor measured from
// the last of them rather than from when it opened — delaying the recovery
// probe by exactly as long as the outage was busy.
func TestAStragglerFailureDoesNotExtendTheOpenWindow(t *testing.T) {
	breaker, clock := newTestBreaker(1, 1, time.Minute)

	_, straggler := breaker.Allow()
	failOnce(breaker, true) // opens the circuit

	clock.Advance(30 * time.Second)
	breaker.Fail(straggler, true) // the straggler finally returns, failing

	clock.Advance(30 * time.Second) // a full minute since it opened
	if !admits(breaker) {
		t.Fatal("the open window must be measured from when the circuit opened, not from the last straggler")
	}
}

// Under concurrency the breaker must still admit exactly the configured number
// of probes. Half-open exists to send one request, not to let the queue that
// built up during the outage back onto an upstream that may still be down.
func TestHalfOpenAdmitsOneProbeUnderConcurrency(t *testing.T) {
	breaker, clock := newTestBreaker(1, 1, time.Minute)
	failOnce(breaker, true)
	clock.Advance(time.Minute)

	const racers = 32
	var start sync.WaitGroup
	start.Add(1)
	var finished sync.WaitGroup
	admittedCount := make([]bool, racers)

	for i := range racers {
		finished.Add(1)
		go func(i int) {
			defer finished.Done()
			start.Wait()
			admittedCount[i], _ = breaker.Allow()
		}(i)
	}
	start.Done()
	finished.Wait()

	got := 0
	for _, ok := range admittedCount {
		if ok {
			got++
		}
	}
	if got != 1 {
		t.Errorf("half-open admitted %d of %d concurrent calls, want exactly 1", got, racers)
	}
}
