package adapters

import "time"

// Config is how an adapter is configured, including the resilience policy it
// applies to its upstream.
//
// The policy is configurable rather than hard-coded because the right bounds
// depend on the deployment: a local E2E stack wants a short circuit window so
// recovery is observable within a test, while production wants one long
// enough that a restart completes before the platform probes again. The
// defaults are the production ones; zero values select them.
type Config struct {
	// BaseURL is the upstream root. Required.
	BaseURL string
	// APIKey is the upstream credential.
	APIKey string
	// Timeout bounds a single attempt.
	Timeout time.Duration
	// MaxBodyBytes bounds how much of a response is read.
	MaxBodyBytes int64
	// RetryAttempts is the total attempts for an idempotent request.
	RetryAttempts int
	// CircuitFailureThreshold is how many consecutive retryable failures open
	// the circuit. A negative value disables the breaker.
	CircuitFailureThreshold int
	// CircuitOpenFor is how long the circuit sheds load before probing.
	CircuitOpenFor time.Duration
}
