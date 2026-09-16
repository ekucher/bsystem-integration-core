# ADR-006: Upstream resilience policy

Status: Accepted

Date: 2026-09-15

## Context

BSYSTEM reads from systems it does not control. Those systems go slow, rate
limit, restart and fail, and the platform's own availability must not be tied
to theirs.

Each adapter originally built its own requests, interpreted its own status
codes and wrapped its own errors. The guarantees the platform makes about
upstream calls therefore held only as strongly as the weakest copy, and every
new adapter would have to re-derive them. Three concrete gaps existed: no
retry of any kind, so a single dropped packet surfaced to the user as a failed
page; no bound on response size, so a broken upstream could exhaust the
platform's memory; and no way to stop calling an upstream that was plainly
down.

## Decision

One shared HTTP layer, `internal/adapters/httpx`, implements the policy once.
Adapters describe *what* they need from an upstream; the layer decides *how*
hard to try.

### Retries

Bounded at three attempts, with exponential backoff from 100ms, capped at 2s,
and full jitter.

Retries are attempted only for failures another attempt could plausibly fix:
rate limits, timeouts and upstream unavailability. Authorization failures and
missing records are deterministic — retrying them only wastes the upstream's
capacity and delays the caller's error.

Jitter is not a refinement. Without it, every caller that failed together
retries together, and the upstream meets the same synchronised burst that
knocked it over.

`Retry-After` is honoured in both header forms, but never beyond the policy's
cap: a mistaken or hostile header must not be able to stall the platform.

### Idempotency

A request is retried only if it declares itself idempotent. This is explicit
rather than derived from the HTTP method, because Outline's RPC API performs
reads over POST — deriving it would either forbid retrying safe Outline reads
or permit retrying genuine writes elsewhere. Every read BSYSTEM performs
declares itself idempotent; nothing else does.

### Circuit breaker

Per adapter: five consecutive retryable failures open the circuit for thirty
seconds, after which one probe decides whether to close it. A failed probe
serves the full period again rather than probing in a tight loop.

Only failures that say something about availability count. A rejected
credential is deterministic: counting it would let a misconfiguration take out
an upstream that is perfectly healthy.

Thirty seconds is short enough that a restart or a rate-limit window passing
is noticed quickly, and long enough that a genuinely broken upstream stops
being called.

### Bounds and safety

Every attempt has a bounded timeout and is cancelled the moment the caller
goes away. A cancelled request is classified separately from an upstream
failure, so a user closing a tab never counts against an upstream.

Response bodies are bounded, and an oversized response is not retried — it
will be oversized again.

Errors name the adapter, the failure kind and the upstream status, and nothing
else. No URL, header, query or response body travels with them, because
adapter errors reach logs and, normalized, the platform boundary.

### Pagination

Collections are requested with a limit and an opaque cursor. The limit is
clamped to what the platform will serve rather than rejected, and the cap is
what stops one request pulling an unbounded amount of upstream data.

The cursor is opaque by contract. It encodes an offset today because every
current upstream paginates by offset, but callers must not read or construct
one: when an upstream gains real cursors, only the cursor codec changes. A
cursor the platform did not issue is rejected rather than reinterpreted.

## Consequences

A transient upstream blip is now invisible to the caller, at the cost of up to
two extra attempts' latency — bounded well inside the adapter timeout, so
retrying can never produce a request that outlives its own deadline.

A sustained outage stops consuming the platform's capacity within five
failures, and the platform reports `502 upstream_unavailable` immediately
instead of making every caller wait for a timeout.

The circuit state is exported to `/metrics` as a gauge per state and surfaced
in `/readyz`, so load shedding is visible rather than silent. An open circuit
does not make the platform unready: the rest of the API keeps working while
one upstream is shed.

Adding an adapter no longer means re-deriving any of this. It means describing
requests and letting the shared layer apply the policy.

## Alternatives considered

**Retry inside each adapter.** This is what drifts. The policy would exist in
three places, and the fourth adapter would get whichever version was copied.

**Derive idempotency from the HTTP method.** Simpler, but wrong for Outline,
whose reads are POSTs. The choice would be between not retrying safe reads and
retrying unsafe writes.

**Retry every failure.** A retried 401 is three rejected credentials instead of
one, and a retried 404 is three lookups for a record that does not exist.
Neither can succeed, and both delay the answer the caller needs.

**No circuit breaker, relying on timeouts alone.** A down upstream would then
cost every caller the full timeout, and the platform's own request capacity
would be consumed waiting on a system already known to be failing.
