# BSYSTEM Integration Core

Integration, authorization-aware data access and event backbone of the BSYSTEM Platform.

## Responsibilities

- normalized API façade for domain services;
- authentik-backed human and service identity handling;
- persistent RBAC and resource scopes;
- Global ID allocation and source mappings;
- immutable audit metadata;
- normalized event ingestion and NATS publishing;
- adapter registry and health aggregation;
- permission-aware data access for future BSYSTEM AI.

Source systems remain authoritative. Integration Core stores platform metadata and mappings, not full shadow databases of CRM, Redmine or Wiki.

## Current adapters

```text
EspoCRM -> Clients / Contacts -> CL-* / CT-*
Redmine -> Projects / Issues  -> PR-* / TSK-*
Outline -> Documents          -> DOC-*
```

Adapters are enabled through environment configuration. Unconfigured adapters remain registered as disabled.

## Human API

```text
GET  /api/v1/me
GET  /api/v1/modules
GET  /api/v1/clients
GET  /api/v1/contacts
GET  /api/v1/projects
GET  /api/v1/issues
GET  /api/v1/documents
POST /api/v1/global-ids
GET  /api/v1/global-ids/{id}
GET  /api/v1/audit
```

Administrative RBAC API:

```text
GET    /api/v1/admin/rbac/roles
GET    /api/v1/admin/rbac/scopes
POST   /api/v1/admin/rbac/scopes
DELETE /api/v1/admin/rbac/scopes
```

## Service API

```text
GET  /api/service/v1/whoami
GET  /api/service/v1/adapters
GET  /api/service/v1/adapters/health
POST /api/service/v1/events
```

Service identities use `SVC-*` Global IDs and a separate service-role mapping.

## Operations

```text
GET /health
GET /readyz
GET /metrics
```

PostgreSQL is required for readiness. NATS is allowed to degrade synchronous reads but its state is surfaced in health/metrics.

## Security boundaries

- authentik proves identity; Integration Core resolves effective authorization;
- browser UI visibility is not authorization enforcement;
- raw upstream errors are not returned to clients;
- customer-facing data must be tenant/scope filtered before exposure;
- the internal Outline document listing rejects Customer access until tenant-aware document scopes exist;
- secrets/tokens/API keys must never enter audit/event payloads;
- BSYSTEM AI must consume authorized Integration Core context rather than upstream databases directly.

## Documentation

- [Architecture](docs/ARCHITECTURE.md)
- [API](docs/API.md)
- [Adapters](docs/ADAPTERS.md)
- [Operations](docs/OPERATIONS.md)
- [AI Gateway Contract](docs/AI-GATEWAY-CONTRACT.md)
- [P0.4 Persistent RBAC & Scopes](docs/P0.4-PERSISTENT-RBAC-SCOPES.md)
