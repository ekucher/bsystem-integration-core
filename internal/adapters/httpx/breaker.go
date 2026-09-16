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

	mu           sync.Mutex
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

// Allow reports whether a call may proceed, moving an expired open circuit to
// half-open on the way.
func (b *Breaker) Allow() bool {
	if !b.enabled() {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if b.settings.now().Sub(b.openedAt) < b.settings.OpenFor {
			b.shortCircuit++
			return false
		}
		b.state = BreakerHalfOpen
		b.transitions++
		b.probes = 1
		b.successes = 0
		return true
	case BreakerHalfOpen:
		if b.probes >= b.settings.HalfOpenProbes {
			b.shortCircuit++
			return false
		}
		b.probes++
		return true
	}
	return true
}

// Succeed records a successful call.
func (b *Breaker) Succeed() {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerHalfOpen:
		b.successes++
		if b.successes >= b.settings.HalfOpenProbes {
			b.state = BreakerClosed
			b.transitions++
			b.failures, b.probes, b.successes = 0, 0, 0
		}
	default:
		b.failures = 0
	}
}

// Fail records a failed call. Only failures that say something about the
// upstream's availability count; pass retryable=false for deterministic ones.
func (b *Breaker) Fail(retryable bool) {
	if !b.enabled() || !retryable {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerHalfOpen:
		// The probe failed, so the upstream has not recovered. Serve the full
		// open period again rather than probing in a tight loop.
		b.state = BreakerOpen
		b.transitions++
		b.openedAt = b.settings.now()
		b.probes, b.successes = 0, 0
	default:
		b.failures++
		if b.failures >= b.settings.FailureThreshold {
			b.state = BreakerOpen
			b.transitions++
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
