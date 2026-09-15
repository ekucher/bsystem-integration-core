# Integration Core Operations

## Health model

Integration Core exposes three operational surfaces:

```text
GET /health
GET /readyz
GET /metrics
```

`/health` reports dependency state for diagnostics. PostgreSQL failure makes the service unhealthy. NATS loss degrades the service but does not block synchronous read APIs.

`/readyz` is intended for orchestrator readiness checks. PostgreSQL is mandatory; NATS is reported but is not a readiness blocker for read traffic.

`/metrics` returns Prometheus text exposition with initial gauges:

```text
bsystem_integration_database_up
bsystem_integration_nats_up
bsystem_adapter_ready{adapter="..."}
```

The metrics endpoint must be exposed only to the monitoring network/reverse proxy in production. The current Compose stack is a DEV baseline, not the final public network policy.

## Adapter health

Authenticated service identities with `adapters.read` can inspect:

```text
GET /api/service/v1/adapters
GET /api/service/v1/adapters/health
```

Disabled adapters are valid configuration state and are not equivalent to an Integration Core failure.

## Event bus

Normalized events are published to:

```text
bsystem.events.<entity.action>
```

Examples:

```text
bsystem.events.backup.failed
bsystem.events.client.updated
bsystem.events.test.failed
```

NATS delivery is asynchronous. Synchronous business reads must not fail merely because NATS is temporarily unavailable.

## Logging

Every HTTP request receives/propagates `X-Request-ID`. Upstream adapter failures are logged server-side while clients receive normalized error codes.

Production logging targets:

```text
JSON structured logs
request_id
service
version
actor/global_id when available
severity
operation
latency
```

Access tokens, passwords, API keys and secrets must never be logged.

## Monitoring baseline

Recommended initial alerts:

- PostgreSQL unavailable;
- readiness returns non-200;
- NATS unavailable for a sustained period;
- configured adapter health becomes degraded;
- elevated 5xx rate;
- repeated authentication failures;
- audit persistence failures.
