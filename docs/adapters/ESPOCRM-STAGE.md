# EspoCRM — stage readiness contract

What the adapter at `internal/adapters/espocrm` actually requires of an EspoCRM
instance, derived from the code rather than from EspoCRM's documentation.

**Compatibility with a live EspoCRM has not been tested.** Every statement below
describes what BSYSTEM sends and expects. The adapter has been exercised only
against the deterministic mock in `bsystem-deploy/mocks`, which was written from
EspoCRM's documented REST conventions. Confirming the real instance agrees is
part of stage acceptance, not a claim made here.

## Connection

| | |
| --- | --- |
| Base URL | `ESPOCRM_URL`, scheme and host required, trailing `/` trimmed |
| Credential | `ESPOCRM_API_KEY`, sent as `X-Api-Key` |
| Enabled when | `ESPOCRM_URL` is non-empty; empty disables the adapter entirely |
| Credential optional | yes — with no key the header is omitted, so an instance with anonymous read would work |

The API key belongs to an EspoCRM **API User**. BSYSTEM only ever reads, so the
role attached to that user needs read access to Accounts and Contacts and
nothing else. Granting more is a larger blast radius for no gain.

## Required capability

Read access to two entity types:

- `Account` — normalized to a BSYSTEM client (`CL-*`)
- `Contact` — normalized to a BSYSTEM contact (`CT-*`)

## Endpoints called

| Purpose | Method | Path |
| --- | --- | --- |
| Health | GET | `/api/v1/App/user` |
| List clients | GET | `/api/v1/Account` |
| One client | GET | `/api/v1/Account/{id}` |
| List contacts | GET | `/api/v1/Contact` |
| One contact | GET | `/api/v1/Contact/{id}` |

Health probes `App/user` because it exercises reachability *and* the credential
in one call: a wrong key fails there rather than at the first business read.

## Pagination

Offset-based, with the window sent as:

```text
maxSize={limit}&offset={offset}&orderBy=name&order=asc
```

Default page 50, hard maximum 200. The cap is BSYSTEM's, not EspoCRM's — it
bounds how much upstream data one request can pull through the adapter.

The ordering is explicit so that paging is stable. An unordered collection can
return the same record on two pages while skipping another.

## Expected response envelope

Collections:

```json
{ "total": 128, "list": [ { "id": "…", "name": "…" } ] }
```

`total` is the size of the whole collection, not of the returned window;
BSYSTEM derives `has_more` from it. A `total` that describes only the window
would make paging terminate one page early.

Detail reads return the record object directly, not wrapped.

## Fields consumed

| BSYSTEM | EspoCRM field | Required |
| --- | --- | --- |
| client id | `id` | yes |
| client name | `name` | yes |
| website | `website` | no |
| email | `emailAddress` | no |
| phone | `phoneNumber` | no |
| contact id | `id` | yes |
| contact name | `name` | yes |
| contact → client | `accountId` | no |

Unknown fields are ignored. A missing optional field decodes to the zero value
and is omitted from the normalized response — it is never rendered as an empty
string that a reader could mistake for a real blank value.

A contact whose `accountId` is absent, or whose account has no mapping, is
returned with no client reference rather than a guessed one. A missing mapping
must not look like a relationship.

## Timeouts

One attempt is bounded by `ADAPTER_TIMEOUT` (default 10s). The deadline lives on
the request context, so retries share one budget and a cancelled caller aborts
the call immediately instead of paying for the rest of the attempts.

Response bodies are capped at 8 MiB; a larger body fails as `KindTooLarge`
rather than being buffered.

## Retries

Every call listed above is a GET and is marked idempotent, so all of them may be
retried: 3 attempts, exponential backoff from 100ms to a 2s ceiling, full
jitter, and `Retry-After` honoured when the upstream sends it.

Retried: rate limited, timeout, unavailable.
Not retried: unauthorized, not found, cancelled, decode failure, oversized body,
circuit open. These are deterministic — another attempt wastes EspoCRM's
capacity and delays the caller's error.

## Circuit breaker

Five consecutive *retryable* failures open the circuit for 30s, then one probe.
A rejected credential or a missing record never trips it: those say nothing
about availability.

While the circuit is open, `/readyz` reports `adapter:espocrm: circuit_open`
but the platform stays ready — the rest of the API keeps working while one
upstream is shed.

## Error translation

Upstream failures never reach HUB verbatim. They normalize to a stable code
with the source named, and the API key is never included in a message, a log
line or a span. This is asserted by a test, not merely intended.

## Version-sensitive assumptions

These are the places where a different EspoCRM version could break the adapter
without any BSYSTEM change:

1. **`X-Api-Key` header name.** Older instructions for EspoCRM describe
   basic-auth API users. If the stage instance is configured that way, the
   adapter authenticates as anonymous and every read fails at health.
2. **`/api/v1/App/user` existence.** Used only for health. If absent, health is
   degraded while business reads still work — a confusing state worth ruling out
   early.
3. **`maxSize` as the page-size parameter.** Some EspoCRM versions and proxies
   accept `limit`. If `maxSize` is ignored, the adapter still works but pages at
   the instance's default size and `has_more` can disagree with reality.
4. **`total` semantics.** If the instance returns the window size rather than
   the collection size, paging stops early and records are silently invisible.
5. **Field names `emailAddress` / `phoneNumber`.** A customized instance may
   expose these under different names; they are optional, so the failure is
   silent absence rather than an error.

Each is a concrete thing to check during stage acceptance; the acceptance
matrix in `bsystem-deploy/docs/STAGE-ACCEPTANCE.md` references this list.
