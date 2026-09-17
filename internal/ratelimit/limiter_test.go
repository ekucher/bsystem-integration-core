package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// at returns a limiter whose clock the test controls. Sleeping for a refill
// makes a test slow and, worse, makes it flaky on a loaded runner — the thing
// being measured is a rate, and a test that waits is measuring the runner too.
func at(perMinute, burst int, clock *time.Time) *Limiter {
	limiter := New(perMinute, burst)
	limiter.now = func() time.Time { return *clock }
	return limiter
}

func TestTheBurstIsWhatTheBurstSays(t *testing.T) {
	clock := time.Now()
	limiter := at(60, 5, &clock)

	for i := 0; i < 5; i++ {
		if allowed, _ := limiter.Allow("USR-000001"); !allowed {
			t.Fatalf("request %d of a burst of 5 was refused", i+1)
		}
	}
	allowed, wait := limiter.Allow("USR-000001")
	if allowed {
		t.Fatal("a sixth request was allowed against a burst of five")
	}
	if wait <= 0 {
		t.Error("a refusal carried no Retry-After; a caller told to wait for nothing will retry immediately")
	}
	if wait > time.Minute {
		t.Errorf("Retry-After = %s: a bound that long is a caller giving up rather than backing off", wait)
	}
}

func TestTokensRefillOverTimeAndDoNotAccumulateForever(t *testing.T) {
	clock := time.Now()
	limiter := at(60, 5, &clock)
	for i := 0; i < 5; i++ {
		limiter.Allow("USR-000001")
	}

	// One second at sixty a minute is one token.
	clock = clock.Add(time.Second)
	if allowed, _ := limiter.Allow("USR-000001"); !allowed {
		t.Error("a token that should have refilled did not")
	}
	if allowed, _ := limiter.Allow("USR-000001"); allowed {
		t.Error("two tokens refilled where one second had passed")
	}

	// An hour of silence must not become an hour's worth of requests. The cap
	// is what makes a burst a burst rather than a savings account.
	clock = clock.Add(time.Hour)
	allowedCount := 0
	for i := 0; i < 50; i++ {
		if allowed, _ := limiter.Allow("USR-000001"); allowed {
			allowedCount++
		}
	}
	if allowedCount != 5 {
		t.Errorf("after an hour idle a principal got %d requests, want the burst of 5", allowedCount)
	}
}

// The property an E2E scenario asserts end to end, at its smallest: one
// principal exhausting their allowance must not cost another theirs.
func TestOnePrincipalCannotSpendAnother(t *testing.T) {
	clock := time.Now()
	limiter := at(60, 3, &clock)

	for i := 0; i < 10; i++ {
		limiter.Allow("USR-000001")
	}
	if allowed, _ := limiter.Allow("USR-000001"); allowed {
		t.Fatal("the noisy principal is not being limited, so the test proves nothing")
	}
	for i := 0; i < 3; i++ {
		if allowed, _ := limiter.Allow("SVC-000009"); !allowed {
			t.Fatalf("a quiet principal was refused request %d because another was noisy", i+1)
		}
	}
}

// Allow must return immediately whether or not it has a token. A limiter that
// waits has turned a rejection into latency, which is the same exhaustion the
// limit exists to prevent, held open one goroutine at a time.
func TestARefusalIsImmediate(t *testing.T) {
	clock := time.Now()
	limiter := at(1, 1, &clock)
	limiter.Allow("USR-000001")

	started := time.Now()
	for i := 0; i < 1000; i++ {
		limiter.Allow("USR-000001")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("a thousand refusals took %s; the limiter is waiting rather than refusing", elapsed)
	}
}

func TestADisabledLimiterAllowsEverything(t *testing.T) {
	for _, limiter := range []*Limiter{New(0, 10), New(100, 0), New(0, 0), nil} {
		if limiter.Enabled() {
			t.Errorf("%+v reports itself enabled", limiter)
		}
		if allowed, wait := limiter.Allow("USR-000001"); !allowed || wait != 0 {
			t.Errorf("a disabled limiter refused a request")
		}
	}
}

// A platform whose principals are machine identities minted per job would
// otherwise grow a bucket per job and release none.
func TestTheNumberOfTrackedKeysIsBounded(t *testing.T) {
	clock := time.Now()
	limiter := at(60, 5, &clock)
	limiter.maxKeys = 100

	for i := 0; i < 5000; i++ {
		limiter.Allow(fmt.Sprintf("SVC-%06d", i))
	}
	if tracked := limiter.Tracked(); tracked > limiter.maxKeys {
		t.Errorf("tracking %d keys with a cap of %d", tracked, limiter.maxKeys)
	}
}

// Buckets nobody has touched are released rather than kept forever.
func TestIdleBucketsAreReleased(t *testing.T) {
	clock := time.Now()
	limiter := at(60, 5, &clock)
	for i := 0; i < 20; i++ {
		limiter.Allow(fmt.Sprintf("USR-%06d", i))
	}
	if limiter.Tracked() != 20 {
		t.Fatalf("tracking %d keys, want 20", limiter.Tracked())
	}

	clock = clock.Add(time.Hour)
	// A new key triggers the sweep, which is when the idle ones go.
	limiter.Allow("USR-999999")
	if tracked := limiter.Tracked(); tracked != 1 {
		t.Errorf("tracking %d keys after an hour of silence, want only the new one", tracked)
	}
}

// The limiter is reached from every request goroutine at once.
func TestTheLimiterIsSafeUnderConcurrentUse(t *testing.T) {
	limiter := New(6000, 500)
	var wait sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for i := 0; i < 200; i++ {
				limiter.Allow(fmt.Sprintf("USR-%06d", i%37))
				limiter.Tracked()
			}
		}(worker)
	}
	wait.Wait()
}
