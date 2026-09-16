package httpx

import (
	"sync"
	"time"
)

// BreakerState is the circuit breaker's state.
type BreakerState string

const (
	// BreakerClosed passes every call through. This is the normal state.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen rejects calls without attempting them, so a failing
	// upstream stops consuming the platform's own capacity.
	BreakerOpen BreakerState = "open"
	// BreakerHalfOpen admits a limited number of probes to discover whether
	// the upstream has recovered.
	BreakerHalfOpen BreakerState = "half_open"
)

// BreakerSettings configures a circuit breaker.
type BreakerSettings struct {
	// FailureThreshold is how many consecutive retryable failures open the
	// circuit. Zero disables the breaker.
	FailureThreshold int
	// OpenFor is how long the circuit stays open before admitting a probe.
	OpenFor time.Duration
	// HalfOpenProbes is how many concurrent probes half-open admits, and how
	// many must succeed to close the circuit again.
	HalfOpenProbes int
	// now is injectable so that tests can drive the clock deterministically
	// rather than sleeping.
	now func() time.Time
}

// DefaultBreakerSettings is the policy adapters use unless told otherwise.
//
// Five consecutive failures is well past a transient blip and short enough
// that a genuinely broken upstream stops being called quickly. Thirty seconds
// open is long enough for a restart or a rate-limit window to pass without
// leaving the platform blind to a recovery for minutes.
func DefaultBreakerSettings() BreakerSettings {
	return BreakerSettings{FailureThreshold: 5, OpenFor: 30 * time.Second, HalfOpenProbes: 1}
}

// Breaker is a per-adapter circuit breaker.
//
// It counts consecutive retryable failures. A deterministic failure — a
// rejected credential, a missing record — says nothing about the upstream's
// availability and never trips it.
type Breaker struct {
	settings BreakerSettings

	mu sync.Mutex
	// episode identifies the run of calls the breaker is currently reasoning
	// about. It changes on every transition, so a result from before the
	// transition can be told apart from one the breaker is waiting for.
	episode      uint64
	state        BreakerState
	failures     int
	probes       int
	successes    int
	openedAt     time.Time
	transitions  int
	shortCircuit int
}

// NewBreaker returns a breaker. A zero FailureThreshold disables it.
func NewBreaker(settings BreakerSettings) *Breaker {
	if settings.HalfOpenProbes <= 0 {
		settings.HalfOpenProbes = 1
	}
	if settings.OpenFor <= 0 {
		settings.OpenFor = 30 * time.Second
	}
	if settings.now == nil {
		settings.now = time.Now
	}
	return &Breaker{settings: settings, state: BreakerClosed}
}

func (b *Breaker) enabled() bool { return b != nil && b.settings.FailureThreshold > 0 }

// Ticket identifies the episode a call was admitted in, so its result can be
// told apart from a result the breaker is no longer waiting for.
//
// Without it, a call still in flight when the circuit opened reports its
// outcome into whatever state the breaker has reached by the time it returns.
// A success arriving during half-open closed the circuit on evidence gathered
// before the upstream was even suspected — and, having reset the failure
// count on the way, left the real probe's failure one short of reopening it.
// The upstream was down, the probe proved it, and the breaker passed full
// traffic through.
//
// Only reachable concurrently, and not exotic: a fan-out against an upstream
// that goes down is the ordinary shape of an outage.
type Ticket uint64

// Allow reports whether a call may proceed, moving an expired open circuit to
// half-open on the way. The ticket must be handed back to Succeed or Fail.
func (b *Breaker) Allow() (bool, Ticket) {
	if !b.enabled() {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerClosed:
		return true, Ticket(b.episode)
	case BreakerOpen:
		if b.settings.now().Sub(b.openedAt) < b.settings.OpenFor {
			b.shortCircuit++
			return false, 0
		}
		b.transition(BreakerHalfOpen)
		b.probes = 1
		b.successes = 0
		return true, Ticket(b.episode)
	case BreakerHalfOpen:
		if b.probes >= b.settings.HalfOpenProbes {
			b.shortCircuit++
			return false, 0
		}
		b.probes++
		return true, Ticket(b.episode)
	}
	return true, Ticket(b.episode)
}

// transition moves to a state and starts a new episode. Callers hold the lock.
func (b *Breaker) transition(to BreakerState) {
	b.state = to
	b.transitions++
	b.episode++
}

// current reports whether a ticket belongs to the episode in progress.
// Callers hold the lock.
func (b *Breaker) current(ticket Ticket) bool { return uint64(ticket) == b.episode }

// Succeed records a successful call. The ticket is the one Allow returned.
func (b *Breaker) Succeed(ticket Ticket) {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// A result from a previous episode describes an upstream the breaker has
	// already made up its mind about. Counting it would let stale evidence
	// decide the current question.
	if !b.current(ticket) {
		return
	}

	switch b.state {
	case BreakerHalfOpen:
		b.successes++
		if b.successes >= b.settings.HalfOpenProbes {
			b.transition(BreakerClosed)
			b.failures, b.probes, b.successes = 0, 0, 0
		}
	default:
		b.failures = 0
	}
}

// Fail records a failed call. Only failures that say something about the
// upstream's availability count; pass retryable=false for deterministic ones.
func (b *Breaker) Fail(ticket Ticket, retryable bool) {
	if !b.enabled() || !retryable {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// As in Succeed: a straggler already had its say. Counting it again would
	// also push openedAt forward on every late arrival, keeping the circuit
	// open for OpenFor measured from the last one rather than from when it
	// opened.
	if !b.current(ticket) {
		return
	}

	switch b.state {
	case BreakerHalfOpen:
		// The probe failed, so the upstream has not recovered. Serve the full
		// open period again rather than probing in a tight loop.
		b.transition(BreakerOpen)
		b.openedAt = b.settings.now()
		b.probes, b.successes = 0, 0
	default:
		b.failures++
		if b.failures >= b.settings.FailureThreshold {
			b.transition(BreakerOpen)
			b.openedAt = b.settings.now()
			b.probes, b.successes = 0, 0
		}
	}
}

// State returns the current state.
func (b *Breaker) State() BreakerState {
	if !b.enabled() {
		return BreakerClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Report the state a caller would actually meet: an open circuit whose
	// window has expired admits the next call.
	if b.state == BreakerOpen && b.settings.now().Sub(b.openedAt) >= b.settings.OpenFor {
		return BreakerHalfOpen
	}
	return b.state
}

// Stats is a snapshot for metrics.
type Stats struct {
	State            BreakerState
	ConsecutiveFails int
	Transitions      int
	ShortCircuited   int
}

// Stats returns a snapshot of the breaker's counters.
func (b *Breaker) Stats() Stats {
	if !b.enabled() {
		return Stats{State: BreakerClosed}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{State: b.state, ConsecutiveFails: b.failures, Transitions: b.transitions, ShortCircuited: b.shortCircuit}
}
