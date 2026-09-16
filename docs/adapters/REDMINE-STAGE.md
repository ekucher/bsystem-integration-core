# Redmine — stage readiness contract

What the adapter at `internal/adapters/redmine` actually requires of a Redmine
instance, derived from the code rather than from Redmine's documentation.

**Compatibility with a live Redmine has not been tested.** The adapter has been
exercised only against the deterministic mock in `bsystem-deploy/mocks`.
Confirming the real instance agrees is part of stage acceptance.

## Connection

| | |
| --- | --- |
| Base URL | `REDMINE_URL`, scheme and host required, trailing `/` trimmed |
| Credential | `REDMINE_API_KEY`, sent as `X-Redmine-API-Key` |
| Enabled when | `REDMINE_URL` is non-empty |
| Credential optional | yes — omitted when empty, so a public Redmine would work |

The REST API must be enabled in Redmine itself (*Administration → Settings →
API → Enable REST web service*). It is off by default, and with it off every
call returns 403 while the instance looks perfectly healthy in a browser — the
single most likely stage blocker in this adapter.

The key belongs to a Redmine user; BSYSTEM sees exactly what that user may see.
A read-only account is enough and is what should be used: the platform never
writes to Redmine.

## Required capability

Read access to:

- `Project` — normalized to a BSYSTEM project (`PR-*`)
- `Issue` — normalized to a BSYSTEM task (`TSK-*`)

## Endpoints called

| Purpose | Method | Path |
| --- | --- | --- |
| Health | GET | `/users/current.json` |
| List projects | GET | `/projects.json` |
| One project | GET | `/projects/{id}.json` |
| List issues | GET | `/issues.json` |
| One issue | GET | `/issues/{id}.json` |

`{id}` may be a numeric id or a project identifier; both are URL-escaped.

## Pagination

Offset-based:

```text
limit={limit}&offset={offset}
```

Default page 50, maximum 100 — Redmine's own ceiling, so asking for more would
be silently clamped upstream and make `has_more` unreliable.

Issue listing additionally sends `status_id=*`. Redmine hides closed issues by
default; BSYSTEM shows the whole collection and lets the caller filter. Without
this, a project's completed work would simply be missing with no error.

Optional `project_id` restricts the collection, applied upstream rather than
after paging.

## Expected response envelope

```json
{ "projects": [ … ], "total_count": 42 }
{ "issues":   [ … ], "total_count": 17 }
```

Detail reads are wrapped in a singular key:

```json
{ "project": { … } }
{ "issue":   { … } }
```

`total_count` must describe the filtered collection, not the returned window.

## Fields consumed

| BSYSTEM | Redmine field | Required |
| --- | --- | --- |
| project id | `id` (integer) | yes |
| project name | `name` | yes |
| project identifier | `identifier` | yes |
| project description | `description` | no |
| project status | `status` (integer) | no |
| task id | `id` (integer) | yes |
| task subject | `subject` | yes |
| task → project | `project.id`, `project.name` | yes |
| task status | `status.id`, `status.name` | yes |

Note that ids are **integers** here, unlike EspoCRM and Outline. A Redmine that
returned them as strings would fail to decode, and the adapter would report
itself degraded rather than silently mis-mapping.

Custom fields are not consumed. Any stage expectation that BSYSTEM reads a
Redmine custom field is unfounded today and would need an explicit change.

## Timeouts, retries, circuit breaker

Identical to the shared HTTP policy: `ADAPTER_TIMEOUT` (default 10s) per
attempt on the request context; 3 attempts with 100ms→2s jittered backoff,
`Retry-After` honoured; retry only on rate-limit, timeout and unavailable;
5 consecutive retryable failures open the circuit for 30s.

All five calls are GETs marked idempotent.

## Error translation

Normalized before reaching HUB. The API key never appears in an error, log or
span.

## Version-sensitive assumptions

1. **REST API disabled.** The default. Produces 403 on every call including
   health. Check this first.
2. **`X-Redmine-API-Key` header.** Redmine also accepts `key=` as a query
   parameter; BSYSTEM deliberately uses the header so the credential never
   lands in an access log or a proxy's URL history.
3. **`/users/current.json` requires authentication.** With no key configured,
   health fails on an instance that permits anonymous project reads — the
   adapter reports degraded while listing would have worked.
4. **`status_id=*`.** Rejected or ignored by some older versions, in which case
   closed issues silently disappear from collections.
5. **`total_count` semantics** with filters applied — as above, a window-sized
   count truncates paging.
6. **Integer ids.** Any deployment or proxy that stringifies ids breaks
   decoding.
