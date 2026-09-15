# BSYSTEM Integration Core

Integration and orchestration backbone of the BSYSTEM Platform.

## Purpose

BSYSTEM Integration Core connects BSYSTEM-HUB, AI services and domain systems without creating direct database coupling between them.

Primary responsibilities:

- API gateway / façade for platform services;
- entity mapping and global IDs;
- event ingestion and routing;
- webhooks and adapters;
- notification routing;
- search indexing;
- audit event collection;
- synchronization workers;
- service registry and health aggregation;
- permission-aware data access for BSYSTEM AI.

## Connected systems

- authentik
- EspoCRM
- Redmine
- BSYSTEM QA
- BSYSTEM Development
- Outline
- BSYSTEM Operations
- BSYSTEM Support
- BSYSTEM AI

## Architectural rules

- No direct cross-module database queries.
- Source systems remain authoritative for their domain data.
- Integration Core stores mappings and integration state, not full copies of source systems.
- External and internal APIs are versioned.
- Events use stable names such as `entity.action`.
- Every request/event should carry correlation metadata.

## Initial P0 scope

- `/health`
- `/api/v1/modules`
- `/api/v1/me` support path
- module/service registry
- global ID mapping skeleton
- structured audit events
- authentik service authentication
- PostgreSQL persistence
- Redis/cache where needed
- NATS-ready event abstraction

See [Architecture](docs/ARCHITECTURE.md) and [Roadmap](docs/ROADMAP.md).
