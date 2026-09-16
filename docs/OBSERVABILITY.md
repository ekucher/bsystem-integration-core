# Observability

Logs and metrics exist to answer questions during an incident. That shapes
what they carry: an identifier that ties a line to a request and to the audit
trail, labels a time series can actually be grouped by, and nothing that turns
a log store into a place secrets accumulate.

## Structured logs

Every line is JSON on stdout, because these lines are read by a log store far
more often than by a person.

```json
{"time":"2026-01-06T11:30:00Z","level":"INFO","msg":"http request",
 "route":"GET /api/v1/clients/{id}","method":"GET","status":200,
 "duration_ms":34,"request_id":"4f1c2a","actor":"USR-000001"}
```

`LOG_LEVEL` selects `debug`, `info`, `warn` or `error`. An unrecognised value
falls back to `info` rather than stopping the service from starting.

### What is logged, and what is not

The `route` is the **registered pattern**, not the request path. A path would
put a resource identifier in every line.

The `actor` is the caller's **Global ID only**. An email or a name in a log
store is personal data the platform has no reason to accumulate, and the
Global ID resolves to the person when that is genuinely needed.

A server error is logged at `error`, a client error at `warn`. A burst of
rejected tokens is the caller's problem, and logging it as an error would make
it read as an outage.

Nothing logs a header map, a query string or a request body. On top of that,
any attribute whose key names a credential — `authorization`, `token`,
`password`, `api_key`, `cookie` and others, matched case-insensitively and by
substring — is replaced with `[REDACTED]` wherever it appears, including
inside groups and on pre-bound loggers. Substring matching costs the occasional
benign field, which is the right side to err on: a redacted field is an
inconvenience, a leaked credential is an incident.

## Metrics

`GET /metrics`, Prometheus text exposition, unauthenticated by design and
carrying no business data.

### HTTP

| Metric | Type | Labels |
| --- | --- | --- |
| `bsystem_http_requests_total` | counter | `route`, `method`, `status`, `status_class` |
| `bsystem_http_request_duration_seconds` | histogram | `route`, `method` |
| `bsystem_http_requests_in_flight` | gauge | `route` |

`route` is the registered pattern. A path label would add a time series for
every Global ID the platform has ever been asked about — expensive to store,
useless to group by, and usually discovered when a monitoring bill arrives.

`status_class` lets an alert match "any server error" without enumerating
codes.

### Adapters

| Metric | Type | Labels |
| --- | --- | --- |
| `bsystem_adapter_attempts_total` | counter | `adapter`, `outcome`, `retry` |
| `bsystem_adapter_attempt_duration_seconds` | histogram | `adapter` |
| `bsystem_adapter_circuit_rejected_total` | counter | `adapter` |
| `bsystem_adapter_ready` | gauge | `adapter` |
| `bsystem_adapter_circuit_state` | gauge | `adapter`, `state` |

`outcome` is the failure kind, or `ok`. Keeping the kinds distinct separates
"the upstream is rate limiting us" from "the upstream is down" from "our
credential is wrong" — three very different pages at three in the morning.

`retry` distinguishes a first attempt from a retry, so a rise in attempts can
be read as either more traffic or more retrying.

The circuit state is a gauge per state rather than a numeric encoding, so an
alert selects `state="open"` instead of remembering which number means open.

### Database and events

| Metric | Type | Labels |
| --- | --- | --- |
| `bsystem_database_up` | gauge | — |
| `bsystem_database_pool_connections` | gauge | `state` |
| `bsystem_database_pool_acquires_total` | counter | `outcome` |
| `bsystem_nats_up` | gauge | — |
| `bsystem_events_published_total` | counter | `event`, `outcome` |

Pool saturation is the metric that explains a slow platform when every
individual query is fast: requests are waiting for a connection, not for the
database. `outcome="empty"` counts acquisitions that had to wait for an empty
pool, which is the early warning.

The pool tallies are sampled from the pool at scrape time rather than mirrored
into a variable, but they are counters, not gauges: they only ever increase
within a process, the name says `_total`, and the dashboard takes `rate()` of
them. They were published as `# TYPE ... gauge` until the contract test below
was written; `rate()` still computed the right answer, because the values were
cumulative regardless of what the type line claimed, but promtool and anything
else that reads the type were being told something untrue.

Event outcomes are counted rather than only logged, because "nothing happened"
and "everything failed to publish" look identical on a dashboard that counts
only successes.

## The tables above are enforced

`cmd/server/metrics_contract_test.go` renders the real registry, records one
observation on every series through its real recording path, and checks the
result against the same names, types and labels listed here. It also refuses a
series that nothing documents.

This exists because the endpoint has a consumer that cannot complain: the
Grafana dashboard in `bsystem-deploy`
(`observability/grafana/bsystem-platform.json`), and any alert built beside it.
A query naming a series that does not exist, or grouping by a label the series
does not carry, returns nothing. Prometheus does not warn and Grafana draws an
empty panel — and during an incident an empty panel reads as "no traffic"
rather than "wrong query", which is the worst available moment to discover a
renamed label.

Two consequences worth knowing before reading a panel as evidence:

- A counter publishes no series until something increments it. On a freshly
  started platform several panels show "No data", and that is the platform
  being new rather than the query being wrong.
- `bsystem_adapter_circuit_state` publishes nothing at all unless some adapter
  has a circuit breaker. The disabled placeholder that stands in for an
  unconfigured integration has none, so on a deployment with no integrations
  configured that panel is empty by design.

## Sampling

Pool sizes, connection states and circuit states are read from whatever owns
them when the registry is rendered, rather than mirrored into a variable that
drifts out of date exactly when it matters. Each sample runs under a bounded
context: a scrape must not block on a dependency that has stopped answering,
since finding out which one that is what the scrape is for.

## Why the registry is written here

The platform needs four metric shapes and a stable exposition. Taking a client
library would pull a large transitive tree into a service that runs with
database and event-bus credentials, and that is a supply-chain decision which
should buy more than this. The exposition format is covered by tests —
cumulative buckets, `+Inf`, `_sum`, `_count`, label escaping and stable
ordering — because output a scraper accepts and then charts incorrectly is
worse than output it rejects.

## Request correlation

`X-Request-ID` is accepted or generated, returned on every response, and
appears in the request log line, the audit trail and every published event. One
identifier traces `HUB → Integration Core → adapter → audit/events`.
