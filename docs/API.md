# BSYSTEM Integration Core API

The machine-readable contract is [`docs/openapi.yaml`](openapi.yaml). It is
contract-tested against the server's own route inventory, so it cannot drift:
an endpoint cannot be added, removed or re-secured without the document
changing with it. This page covers the conventions behind that contract.

## Purpose

Integration Core is the controlled integration boundary between BSYSTEM-HUB and
external/domain systems. It stores platform metadata only and must not replace
source-of-truth systems such as CRM, Redmine, QA or Operations.

## Surfaces

| Surface | Base path | Authentication |
| --- | --- | --- |
| Human API | `/api/v1` | user access token from authentik |
| Machine API | `/api/service/v1` | service identity in `BSYSTEM-Services` |
| Operations | `/health`, `/readyz`, `/metrics` | none |

The two API surfaces are kept separate on purpose: a human token cannot reach
the machine API, and a service token cannot reach the human API.

### Endpoint index

| Endpoint | Requires |
| --- | --- |
| `GET /api/v1/me` | any authenticated user |
| `GET /api/v1/modules` | any authenticated user |
| `GET /api/v1/clients` | `crm.client.read` |
| `GET /api/v1/contacts` | `crm.client.read` |
| `GET /api/v1/projects` | `projects.task.read` |
| `GET /api/v1/issues` | `projects.task.read` |
| `GET /api/v1/documents` | `wiki.document.read`, and not the Customer role |
| `POST /api/v1/global-ids` | administrator |
| `GET /api/v1/global-ids/{id}` | administrator |
| `GET /api/v1/audit` | administrator |
| `GET /api/v1/admin/rbac/roles` | administrator |
| `GET|POST|DELETE /api/v1/admin/rbac/scopes` | administrator |
| `GET /api/service/v1/whoami` | service identity |
| `GET /api/service/v1/adapters` | `adapters.read` |
| `GET /api/service/v1/adapters/health` | `adapters.read` |
| `POST /api/service/v1/events` | `events.publish` |
| `GET /health`, `GET /readyz`, `GET /metrics` | none |

## Authentication

Human requests use an OIDC access token issued by authentik and forwarded by
BSYSTEM-HUB:

```http
Authorization: Bearer <access_token>
```

Integration Core validates the token against the authentik UserInfo endpoint,
persists the identity mapping and resolves BSYSTEM RBAC. No human password is
accepted by Integration Core.

Service identities present the same kind of token but must be members of
`BSYSTEM-Services`; a token outside that group is refused by the machine API.

## Authorization

Authorization is enforced in the backend for every request and denies by
default. A principal whose groups map to no role resolves to no permissions and
no modules, and is refused everywhere.

An unknown or unmapped resource results in denial, never in broader access. The
Customer role is refused unscoped documents with `scope_required`, because the
tenant ownership boundary is not yet authoritative — an undecided boundary must
deny rather than disclose.

## Request correlation

`X-Request-ID` is accepted on every request. If missing, Integration Core
creates one. It is returned on every response and reaches the audit trail and
every published event, so a single identifier traces
`HUB → Integration Core → adapter → audit/events`.

## Errors

Every failure uses one shape:

```json
{
  "error": "upstream service unavailable",
  "code": "upstream_unavailable",
  "source": "espocrm"
}
```

`error` is always present. `code` is the stable machine-readable form where one
is defined; `source` names the failing adapter.

An upstream failure — an error status, a rate limit or a timeout — normalizes to
`502` with `upstream_unavailable`. Upstream status codes, hostnames, credentials
and payloads are never forwarded, and no error body carries a credential, an
internal hostname, a stack trace or an upstream payload.

## Normalized entities

Normalized entities never re-expose a raw upstream payload. Each carries its
platform Global ID alongside the `source` and `source_id` it came from, so the
source system stays authoritative and traceable:

```json
{
  "id": "CL-000001",
  "source": "espocrm",
  "source_id": "acc-northwind",
  "name": "Northwind Trading"
}
```

A reference that cannot be resolved is omitted rather than guessed: a contact
whose upstream account has no mapping is returned without a `client_id`.

## Global IDs

`POST /api/v1/global-ids` creates or returns an idempotent mapping from a
source-system entity to a BSYSTEM Global ID. The same
`(source, entity_type, source_id)` always resolves to the same Global ID, and a
Global ID is never reassigned.

| Entity | Prefix |
|---|---|
| User | `USR` |
| Service identity | `SVC` |
| Client | `CL` |
| Contact | `CT` |
| Project | `PR` |
| Task | `TSK` |
| Server | `SRV` |
| Application | `APP` |
| Incident | `INC` |
| Test Case | `TST` |
| Bug | `BUG` |
| Document | `DOC` |
| Release | `REL` |
| Repository | `REP` |

Identity is never derived from names, hostnames, emails or other mutable
labels.

## Pagination

Collections accept `limit`. Values above the maximum are clamped rather than
rejected. Cursor-based pagination is not part of this version; collections are
bounded by `limit` alone.

## Audit

The audit trail is administrator-only and records who read and changed what:
the OIDC subject, the `USR-*` Global ID, the action in `entity.action` naming,
the resource type and Global ID, the `X-Request-ID`, the source IP, metadata
and a timestamp.

## Events

Platform events are published to NATS on `bsystem.events.<event>` using
`entity.action` naming. The API keeps serving requests when NATS is
unavailable; only publication is affected.

Envelopes are normalized before publication: `actor_id` defaults to the
publishing identity, `request_id` to the request correlation id, `occurred_at`
to the time of publication and `severity` to `info`.

Platform-generated events:

```text
identity.created
service_identity.created
global_id.created
```

Business events keep the same convention:

```text
client.created
project.updated
task.completed
bug.created
test.failed
build.failed
release.created
server.offline
backup.failed
incident.created
document.updated
```

## Persistence boundaries

Integration Core owns only:

- identity and service identity mappings;
- module registry;
- Global ID counters and mappings;
- RBAC roles, permissions and scope grants;
- audit events;
- integration metadata.

It does **not** own CRM clients, Redmine tasks, QA test cases, Outline documents
or Operations metrics.

## Failure isolation

A failed source adapter must not make unrelated adapters unavailable.
PostgreSQL is a required dependency; NATS is auxiliary, and its loss degrades
event delivery without making the API unavailable.
