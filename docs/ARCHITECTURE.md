# Integration Core Architecture

## Position in BSYSTEM Platform

```text
BSYSTEM-HUB
    │
    ▼
Integration Core
    ├── CRM adapter
    ├── Projects adapter
    ├── QA adapter
    ├── Wiki adapter
    ├── Operations adapter
    ├── Support adapter
    └── AI-safe data access
```

## Domain ownership

Authoritative sources remain external to Integration Core:

```text
User          → authentik
Client        → CRM
Project/Task  → Redmine
Test Case/Bug → BSYSTEM QA
Document      → Outline
Health Event  → Operations
Incident      → Support
```

Integration Core stores only what is necessary to connect these systems.

## Core components

### API Gateway / Façade

Provides normalized BSYSTEM APIs to HUB and AI.

Initial namespace:

```text
/api/v1
```

### Entity Mapping

Maintains immutable BSYSTEM global IDs and source identifiers.

Example:

```json
{
  "global_id": "PR-000103",
  "source": "redmine",
  "source_id": "428"
}
```

### Event Bus abstraction

Event naming convention:

```text
client.created
project.updated
task.completed
bug.created
backup.failed
incident.created
document.updated
```

Initial preferred transport: NATS. The application layer must not depend on transport-specific semantics where avoidable.

### Webhooks and adapters

Each external system receives a dedicated adapter boundary. Upstream-specific code must not leak into platform domain models.

### Search indexing

Integration Core produces normalized search documents for OpenSearch/Elasticsearch or another selected backend.

### Audit

Audit records must include actor, action, target, timestamp, source, request/correlation ID and result.

### AI data boundary

BSYSTEM AI requests normalized data through Integration Core. Access is filtered by user/service identity and scope before data is passed to a model.

## Persistence

PostgreSQL stores:

- global IDs;
- source mappings;
- module registry;
- webhook registrations/state;
- synchronization checkpoints;
- integration metadata;
- audit metadata where applicable.

Redis may be used for caching, distributed locks, short-lived state and rate
limiting. Nothing uses it today, and `bsystem-deploy` no longer provisions one:
the stack carried a Redis container that no service ever talked to, which is
worse than none, because a running container reads as a dependency in use. The
first feature that needs Redis adds the service back along with the use.

## Reliability rules

- idempotent event handlers where practical;
- retry with bounded backoff;
- dead-letter handling for failed asynchronous work;
- no silent data loss;
- source-system outage must degrade only affected integrations;
- correlation IDs propagated end-to-end.
