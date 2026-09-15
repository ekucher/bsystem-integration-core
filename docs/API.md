# BSYSTEM Integration Core API

Base path: `/api/v1`

## Purpose

Integration Core is the controlled integration boundary between BSYSTEM-HUB and external/domain systems. It stores platform metadata only and must not replace source-of-truth systems such as CRM, Redmine, QA or Operations.

## Authentication

Human requests use an OIDC access token issued by authentik and forwarded by BSYSTEM-HUB:

```http
Authorization: Bearer <access_token>
```

Integration Core validates the token against the authentik UserInfo endpoint, persists the identity mapping and resolves BSYSTEM RBAC.

No human password is accepted by Integration Core.

## Request correlation

`X-Request-ID` is accepted on every request. If missing, Integration Core creates one and returns it in the response.

## GET /health

Public liveness/dependency status.

```json
{
  "status": "ok",
  "service": "bsystem-integration-core",
  "version": "0.3.0",
  "timestamp": "2026-09-15T20:00:00Z",
  "checks": {
    "database": "ok",
    "nats": "ok"
  }
}
```

Database failure returns HTTP 503. NATS failure degrades event delivery but does not make the API unavailable.

## GET /api/v1/me

Returns the authenticated user's persistent Global ID and effective access model.

```json
{
  "id": "USR-000001",
  "subject": "authentik-subject",
  "email": "developer@example.com",
  "name": "Developer",
  "username": "developer",
  "groups": ["BSYSTEM-Developers"],
  "roles": ["Developer"],
  "permissions": ["projects.task.read", "development.pr.write"],
  "modules": ["projects", "qa", "development", "wiki", "operations"]
}
```

The `USR-*` identifier is allocated once and remains stable even if email, username or group membership changes.

## GET /api/v1/modules

Returns only enabled modules allowed by the caller's effective roles. Module metadata comes from PostgreSQL `modules`, not from a frontend hard-coded list.

## POST /api/v1/global-ids

Administrator-only in P0.2.

Creates or returns an idempotent mapping from a source-system entity to a BSYSTEM Global ID.

```json
{
  "entity_type": "client",
  "source": "espocrm",
  "source_id": "65fa1234",
  "tenant_id": "CL-000042",
  "metadata": {
    "name": "Example Client"
  }
}
```

Response:

```json
{
  "global_id": "CL-000043",
  "entity_type": "client",
  "source": "espocrm",
  "source_id": "65fa1234",
  "tenant_id": "CL-000042",
  "metadata": {
    "name": "Example Client"
  },
  "created_at": "2026-09-15T20:00:00Z"
}
```

The same `(source, entity_type, source_id)` must resolve to the same Global ID.

Supported P0.2 prefixes:

| Entity | Prefix |
|---|---|
| User | `USR` |
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

## GET /api/v1/global-ids/{global_id}

Administrator-only in P0.2. Resolves a Global ID to its authoritative source mapping.

## GET /api/v1/audit?limit=100

Administrator-only in P0.2. Returns the newest immutable audit records. Maximum page size is 500 during P0.

Audit records include:

- OIDC subject;
- BSYSTEM `USR-*` ID;
- action;
- resource type and Global ID;
- `X-Request-ID`;
- source IP;
- metadata;
- timestamp.

## NATS events

P0.2 publishes platform events using NATS subjects. The application must continue serving API requests if NATS is temporarily unavailable.

Initial events:

```text
identity.created
global_id.created
```

Future business events keep the `entity.action` convention:

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

- identity mappings;
- module registry;
- Global ID counters and mappings;
- audit events;
- integration metadata.

It does **not** own CRM clients, Redmine tasks, QA test cases, Outline documents or Operations metrics.

## Failure isolation

A failed source adapter must not make unrelated adapters unavailable. PostgreSQL is currently a required dependency; Redis and NATS are auxiliary platform dependencies.
