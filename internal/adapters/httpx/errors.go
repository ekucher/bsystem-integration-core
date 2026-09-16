// Package httpx is the shared HTTP layer every BSYSTEM adapter is built on.
//
// It exists so that the guarantees BSYSTEM makes about upstream calls —
// bounded timeouts, context cancellation, bounded response sizes, bounded and
// idempotency-safe retries, a circuit breaker, typed errors and
// credential-safe diagnostics — hold identically for every upstream, rather
// than being re-implemented once per adapter and drifting.
package httpx

import (
	"errors"
	"fmt"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

// Kind classifies an upstream failure. Handlers branch on the kind rather
// than on an upstream status code, so the platform's own responses stay
// independent of any one upstream's conventions.
type Kind string

const (
	// KindNotFound means the upstream has no such record.
	KindNotFound Kind = "not_found"
	// KindUnauthorized means the upstream rejected the adapter's credential.
	KindUnauthorized Kind = "unauthorized"
	// KindRateLimited means the upstream asked the platform to slow down.
	KindRateLimited Kind = "rate_limited"
	// KindTimeout means the upstream did not answer within the bound.
	KindTimeout Kind = "timeout"
	// KindCanceled means the caller went away; the upstream is not at fault.
	KindCanceled Kind = "canceled"
	// KindCircuitOpen means the adapter is shedding load after repeated
	// failures and did not attempt the call.
	KindCircuitOpen Kind = "circuit_open"
	// KindUnavailable covers any other upstream failure.
	KindUnavailable Kind = "unavailable"
	// KindDecode means the upstream answered with something unreadable.
	KindDecode Kind = "decode"
	// KindTooLarge means the upstream response exceeded the configured bound.
	KindTooLarge Kind = "too_large"
)

// Error is an upstream failure.
//
// Its message names the adapter, the kind and — where one exists — the
// upstream status code. It deliberately carries no URL, no header and no
// response body, because adapter errors reach logs and, in normalized form,
// the platform boundary. A credential or an internal hostname must not travel
// with them.
type Error struct {
	Adapter string
	Kind    Kind
	// Status is the upstream HTTP status, or zero when the call never
	// produced one.
	Status int
	// RetryAfter is the delay the upstream asked for, when it asked.
	RetryAfter time.Duration
	// cause is kept for operators but is never rendered into the message,
	// because a transport error can quote the request URL.
	cause error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s upstream %s (HTTP %d)", e.Adapter, e.Kind, e.Status)
	}
	return fmt.Sprintf("%s upstream %s", e.Adapter, e.Kind)
}

// Unwrap exposes the transport cause to errors.Is checks for sentinel values
// such as context.DeadlineExceeded. It is not part of the message.
func (e *Error) Unwrap() error { return e.cause }

// Is maps the typed kinds onto the package-level sentinels the rest of the
// platform matches against.
func (e *Error) Is(target error) bool {
	switch target {
	case adapters.ErrNotFound:
		return e.Kind == KindNotFound
	case ErrRetryable:
		return e.retryable()
	}
	return false
}

// ErrRetryable matches any error the client would retry, for tests and for
// callers that want to reason about retry behaviour without re-deriving it.
var ErrRetryable = errors.New("retryable upstream failure")

// retryable reports whether another attempt could plausibly succeed.
//
// Authorization and not-found failures are deterministic: retrying them
// wastes the upstream's capacity and delays the caller's error. A cancelled
// request is not retried because the caller is already gone.
func (e *Error) retryable() bool {
	switch e.Kind {
	case KindRateLimited, KindTimeout, KindUnavailable:
		return true
	case KindNotFound, KindUnauthorized, KindCanceled, KindCircuitOpen, KindDecode, KindTooLarge:
		return false
	}
	return false
}

// KindOf returns the kind of an upstream error, or an empty kind when err did
// not come from this package.
func KindOf(err error) Kind {
	var upstream *Error
	if errors.As(err, &upstream) {
		return upstream.Kind
	}
	return ""
}

// kindForStatus classifies an upstream status code.
func kindForStatus(status int) Kind {
	switch {
	case status == 401 || status == 403:
		return KindUnauthorized
	case status == 404:
		return KindNotFound
	case status == 408:
		return KindTimeout
	case status == 429:
		return KindRateLimited
	case status >= 500:
		return KindUnavailable
	default:
		return KindUnavailable
	}
}
