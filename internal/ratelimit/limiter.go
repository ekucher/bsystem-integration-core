// Package ratelimit bounds how often one principal may reach an expensive
// surface.
//
// It is deliberately per instance and in memory. A shared counter in Redis
// would make the limit exact across replicas, and it would also add a
// dependency whose outage has to be answered — fail open and the limit is
// gone, fail closed and the platform is gone. Nothing in this deployment has
// demonstrated the need for an exact limit, and an approximate one enforced
// n times for n replicas still bounds abuse, so the dependency is not taken.
// That decision is recorded in docs/RATE-LIMITS.md rather than left implicit.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a token bucket per key.
//
// The key is always a principal the platform has already authenticated: a
// USR-* or a SVC-*. It is never a header, a username or an address. A limiter
// keyed on something the caller controls is a limiter the caller can step
// around by changing it, and one keyed on a client address throttles everybody
// behind a shared egress together.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	// idle is how long a bucket with no traffic is kept. A principal who has
	// stopped calling should not cost memory forever, and one who comes back
	// after this long starts full — which is the same state they would have
	// refilled into anyway.
	idle    time.Duration
	maxKeys int

	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// New returns a limiter allowing burst requests immediately and perMinute
// sustained. A burst or rate of zero disables it.
func New(perMinute, burst int) *Limiter {
	return &Limiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		idle:    10 * time.Minute,
		maxKeys: 10000,
		now:     time.Now,
		buckets: map[string]*bucket{},
	}
}

// Enabled reports whether this limiter does anything. A disabled limiter is
// how a deployment turns a class off, and it must be visibly disabled rather
// than configured with a number so large it never triggers.
func (l *Limiter) Enabled() bool { return l != nil && l.rate > 0 && l.burst > 0 }

// Allow reports whether this key may proceed, and how long to wait if not.
//
// It never blocks. A limiter that waits for a token has turned a rejection
// into latency, which is the same resource exhaustion the limit exists to
// prevent, held open one goroutine at a time.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if !l.Enabled() {
		return true, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, present := l.buckets[key]
	if !present {
		l.sweepLocked(now)
		entry = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = entry
	} else {
		// Refill for the time that passed, capped at the burst. Capping is
		// what stops an idle principal accumulating a week of requests.
		elapsed := now.Sub(entry.seen).Seconds()
		if elapsed > 0 {
			entry.tokens += elapsed * l.rate
			if entry.tokens > l.burst {
				entry.tokens = l.burst
			}
		}
		entry.seen = now
	}

	if entry.tokens >= 1 {
		entry.tokens--
		return true, 0
	}
	// How long until one whole token exists. Rounded up, and never zero: a
	// Retry-After of 0 invites an immediate retry that will be refused again.
	wait := time.Duration((1 - entry.tokens) / l.rate * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// sweepLocked drops buckets nobody has used, and evicts the stalest if the map
// is still too large.
//
// Without a bound, a platform whose principals are machine identities minted
// per job would grow a bucket per job and never release one. With it, the
// worst case is that a very busy platform forgets a bucket early and that
// principal starts full — the limit is approximate by design, and this is one
// of the ways.
func (l *Limiter) sweepLocked(now time.Time) {
	if len(l.buckets) < l.maxKeys {
		for key, entry := range l.buckets {
			if now.Sub(entry.seen) > l.idle {
				delete(l.buckets, key)
			}
		}
		return
	}
	var stalestKey string
	var stalest time.Time
	for key, entry := range l.buckets {
		if now.Sub(entry.seen) > l.idle {
			delete(l.buckets, key)
			continue
		}
		if stalest.IsZero() || entry.seen.Before(stalest) {
			stalestKey, stalest = key, entry.seen
		}
	}
	if len(l.buckets) >= l.maxKeys && stalestKey != "" {
		delete(l.buckets, stalestKey)
	}
}

// Tracked reports how many keys the limiter is holding. It exists so the
// memory bound can be asserted rather than assumed.
func (l *Limiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
