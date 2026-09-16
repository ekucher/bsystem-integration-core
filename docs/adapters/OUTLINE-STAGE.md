# Outline — stage readiness contract

What the adapter at `internal/adapters/outline` actually requires of an Outline
instance, derived from the code rather than from Outline's documentation.

**Compatibility with a live Outline has not been tested.** The adapter has been
exercised against the deterministic mock in `bsystem-deploy/mocks` and against
unit tests rewritten around Outline's *documented* envelope after a real defect
was found there — see "History" below. Confirming a live instance agrees is part
of stage acceptance.

## Connection

| | |
| --- | --- |
| Base URL | `OUTLINE_URL`, scheme and host required, trailing `/` trimmed |
| Credential | `OUTLINE_API_KEY`, sent as `Authorization: Bearer <key>` |
| Enabled when | `OUTLINE_URL` is non-empty |
| Credential optional | **no** — construction fails without a key |

Outline is the one adapter where the key is mandatory rather than optional.
Outline has no anonymous read surface, so a missing key is a configuration
error worth failing loudly at startup instead of a runtime 401 on first use.

The key should be scoped to read-only access to the collections BSYSTEM is
meant to surface. Outline API keys are user-scoped: BSYSTEM sees exactly the
documents that user can see, which is also the isolation boundary to verify
during acceptance.

## Required capability

Read and search access to documents, normalized to BSYSTEM documents (`DOC-*`).

## Endpoints called

Outline is RPC over `POST`, not REST.

| Purpose | Method | Path | Body |
| --- | --- | --- | --- |
| Health | POST | `/api/documents.list` | `{"limit": 1}` |
| List documents | POST | `/api/documents.list` | `{"limit", "offset"}` |
| Search documents | POST | `/api/documents.search` | `{"query", "limit", "offset"}` |
| One document | POST | `/api/documents.info` | `{"id"}` |

Health asks for a single document rather than a page: it exercises reachability
and the credential without pulling content.

Note that these are POSTs. They are nonetheless **reads**, and are marked
idempotent so the retry policy applies — an RPC-shaped read is still safe to
repeat.

## Pagination

Offset-based, in the request body. Default page 50, maximum 100 (Outline's own
ceiling).

## Expected response envelope

```json
{ "data": [ … ], "pagination": { "offset": 0, "limit": 25, "total": 4 } }
```

The collection is an **array under `data`**, with paging metadata alongside it.
Detail reads return the object under `data` directly.

Search results are ranked wrappers, not bare documents:

```json
{ "data": [ { "context": "…", "ranking": 0.8, "document": { … } } ] }
```

## Fields consumed

| BSYSTEM | Outline field | Required |
| --- | --- | --- |
| document id | `id` | yes |
| title | `title` | yes |
| body | `text` | no |
| link | `url` | no |
| collection | `collectionId` | no |
| updated at | `updatedAt` | no |
| search snippet | `context` | no |
| search rank | `ranking` | no |

Missing optional fields are omitted from the normalized response rather than
rendered as empty values.

## Timeouts, retries, circuit breaker

The shared policy: `ADAPTER_TIMEOUT` (default 10s) per attempt; 3 attempts,
100ms→2s jittered backoff, `Retry-After` honoured; retry only on rate-limit,
timeout and unavailable; 5 consecutive retryable failures open the circuit for
30s. Bodies capped at 8 MiB — relevant here, because document `text` can be
large.

## Error translation

Normalized before reaching HUB. A test asserts that an upstream failure never
surfaces the API key.

## History: the envelope defect

The adapter originally modelled `documents.list` as
`{"data": {"documents": [...]}}`. Outline returns the array directly under
`data`. Every real response therefore failed to decode: the adapter reported
itself degraded and `GET /api/v1/documents` normalized to `502
upstream_unavailable` against any genuine Outline instance.

The existing unit test did not catch it because its fake upstream had been
written to match the implementation rather than the documented API. That is the
failure mode worth remembering when reading the rest of this document: a test
written against your own assumption proves only that the assumption is
self-consistent. The tests were rewritten around the documented envelope.

This is also why nothing here claims live compatibility.

## Version-sensitive assumptions

1. **`data` is an array for collections.** The defect above. A future or
   self-hosted variant that wraps it differently breaks every list read.
2. **`/api` path prefix** on RPC methods.
3. **Bearer authentication.** Outline has used other schemes historically.
4. **`pagination.total`** present and describing the whole collection.
5. **Search result shape** — a flat document array instead of ranked wrappers
   would decode to empty results rather than erroring.
6. **Document size.** A document body beyond 8 MiB fails as oversized rather
   than truncating.
