# BSYSTEM Integration Core API

Base path: `/api/v1`

## Purpose

Integration Core is the controlled integration boundary between BSYSTEM-HUB and external/domain systems. It must not become a replacement for the source-of-truth databases.

## Core responsibilities

- entity mapping
- global ID resolution
- adapters
- event ingestion
- webhook handling
- audit propagation
- search indexing
- controlled AI data access

## Initial endpoints

### GET /health

```json
{
  "status": "ok",
  "service": "bsystem-integration-core",
  "version": "0.1.0",
  "timestamp": "2026-09-15T19:00:00Z"
}
```

### GET /mappings/{global_id}

Returns source mappings for a BSYSTEM entity.

### POST /mappings

Creates an explicit mapping between a BSYSTEM global ID and a source-system entity.

Example:

```json
{
  "global_id": "CL-000042",
  "entity_type": "client",
  "source": "espocrm",
  "source_id": "65fa1234"
}
```

### POST /events

Accepts normalized platform events.

```json
{
  "event": "backup.failed",
  "source": "operations",
  "entity_id": "SRV-000017",
  "client_id": "CL-000042",
  "severity": "error",
  "timestamp": "2026-09-15T19:00:00Z",
  "data": {
    "message": "VSS snapshot failed"
  }
}
```

## Event naming

Format:

```text
entity.action
```

Examples:

- `client.created`
- `project.updated`
- `task.completed`
- `bug.created`
- `test.failed`
- `build.failed`
- `release.created`
- `server.offline`
- `backup.failed`
- `incident.created`
- `document.updated`

## Request correlation

`X-Request-ID` must be accepted and forwarded to downstream adapters where possible.

## Authentication

Human access is indirect through BSYSTEM-HUB. Service-to-service requests use dedicated service identities/tokens. No human password is ever accepted by Integration Core.

## Failure isolation

If a source system is unavailable, Integration Core should return a dependency-specific error and must not cause unrelated adapters to fail.
