# Adapters

An adapter isolates HUB and the normalized API from one upstream system.
Nothing outside an adapter package sees an upstream field name, status code or
envelope — that is what keeps the platform's contract independent of any
source system's conventions.

## Registered adapters

| Adapter | Upstream | Capabilities |
| --- | --- | --- |
| `espocrm` | EspoCRM REST API | `clients.read`, `contacts.read` |
| `redmine` | Redmine REST API | `projects.read`, `issues.read` |
| `outline` | Outline RPC API | `documents.read`, `documents.search` |

Each is configured by a URL and an API key from the environment. An adapter
whose URL is unset registers as `disabled` rather than being absent, so the
registry always describes the whole intended surface.

## Common interface

```go
Info() adapters.Info          // id, name, version, status, capabilities
Health(ctx) adapters.Health   // a live probe, with a credential-safe message
BreakerState() string         // closed, half_open or open
```

`Health` probes an endpoint that exercises both reachability and the
credential, so a wrong key is reported as degraded rather than discovered on
the first business request.

## Shared HTTP behaviour

Every adapter is built on `internal/adapters/httpx`, which implements the
resilience policy once. See
[ADR-006](adr/ADR-006-upstream-resilience-policy.md) for the reasoning.

| Guarantee | Behaviour |
| --- | --- |
| Timeouts | every attempt is bounded; the default is 10s |
| Cancellation | a caller going away aborts the call immediately and is not counted as an upstream failure |
| Body size | responses are bounded; an oversized response is not retried |
| Retries | at most 3 attempts, exponential backoff from 100ms capped at 2s, full jitter |
| Retry scope | only rate limits, timeouts and unavailability; never authorization or not-found |
| Idempotency | explicit per request, because Outline reads over POST |
| `Retry-After` | honoured in both header forms, never beyond the cap |
| Circuit breaker | opens after 5 consecutive retryable failures, 30s, then one probe |
| Errors | name the adapter, kind and status only — no URL, header, query or upstream body |

### Failure kinds

Handlers branch on the kind, never on an upstream status code:

| Kind | Platform response |
| --- | --- |
| `not_found` | `404 not_found` |
| `unauthorized`, `rate_limited`, `timeout`, `unavailable`, `circuit_open`, `decode`, `too_large` | `502 upstream_unavailable` |
| `canceled` | the caller is already gone |

`not_found` satisfies `adapters.ErrNotFound`, so a handler can answer `404`
without knowing which adapter it is talking to.

## Pagination

Collections take an `adapters.Page` and return the items plus an
`adapters.PageInfo`:

```go
ListAccounts(ctx, adapters.Page{Limit: 50}) ([]Account, adapters.PageInfo, error)
```

`PageInfo.NextCursor` is empty once the collection is exhausted, so walking it
always terminates. The cursor is opaque: it encodes an offset today, but
callers must not read or construct one.

Page sizes are clamped to what the platform will serve — 200 for CRM
collections, 100 for Redmine and Outline, matching those systems' own limits.

## Observability

The circuit state of every adapter is exported to `/metrics`:

```text
bsystem_adapter_ready{adapter="espocrm"} 1
bsystem_adapter_circuit_state{adapter="espocrm",state="closed"} 1
bsystem_adapter_circuit_state{adapter="espocrm",state="half_open"} 0
bsystem_adapter_circuit_state{adapter="espocrm",state="open"} 0
```

A state is exported as a gauge per state rather than a numeric encoding, so an
alert can select `state="open"` instead of remembering which number means
open. A non-closed circuit also appears in `/readyz`, without making the
platform unready: the rest of the API keeps working while one upstream is
shed.

## Security

- API keys are read from the environment and never logged.
- Adapter errors carry no credential, URL, header or upstream payload. A test
  asserts this directly.
- Upstream error bodies are drained but never read into an error, because an
  upstream error page can contain internal hostnames and confidential content.

## Adding an adapter

1. Build an `httpx.Client` in `New`, and keep credentials in a header value
   the package owns.
2. Implement `Info`, `Health` and `BreakerState`.
3. Model only the upstream fields BSYSTEM normalizes, with the upstream's own
   JSON names, so the mapping stays visible in one place.
4. Mark reads `Idempotent` so they may be retried.
5. Return `adapters.Page`-shaped collections.
6. Add contract tests against a deterministic fake upstream, including the
   failure paths and a check that errors do not leak the credential.

The E2E stack in `bsystem-deploy` runs the adapter against a mock upstream
that serves the documented response shapes — which is how the Outline
envelope bug was found after its own unit test had missed it.
