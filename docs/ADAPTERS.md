# BSYSTEM Integration Adapters

## Purpose

Adapters isolate BSYSTEM from upstream implementation details. Integration Core exposes normalized entities and stable Global IDs instead of leaking third-party APIs into HUB.

## Configuration

Adapters are enabled only when their URL is configured. Missing configuration keeps the adapter registered as `disabled`.

### EspoCRM

Environment:

```text
ESPOCRM_URL
ESPOCRM_API_KEY
```

Capabilities:

```text
clients.read
contacts.read
```

Normalized API:

```text
GET /api/v1/clients
GET /api/v1/contacts
```

Mappings:

```text
EspoCRM Account -> CL-*
EspoCRM Contact -> CT-*
```

### Redmine

Environment:

```text
REDMINE_URL
REDMINE_API_KEY
```

Capabilities:

```text
projects.read
issues.read
```

Normalized API:

```text
GET /api/v1/projects
GET /api/v1/issues?project=<identifier>
```

Mappings:

```text
Redmine Project -> PR-*
Redmine Issue   -> TSK-*
```

### Outline

Environment:

```text
OUTLINE_URL
OUTLINE_API_KEY
```

Use a read-only/scoped Outline API key when possible.

Capability:

```text
documents.read
```

Normalized API:

```text
GET /api/v1/documents
```

Mapping:

```text
Outline Document -> DOC-*
```

Customer users are intentionally denied access to the internal document-list endpoint until tenant-aware document scopes are implemented.

## Error boundary

Raw upstream errors are logged server-side. Human API clients receive a stable envelope such as:

```json
{
  "error": "upstream service unavailable",
  "code": "upstream_unavailable",
  "source": "espocrm"
}
```

Secrets, API keys and access tokens must never be included in normalized responses, audit metadata or events.

## Adapter rules

1. Outbound requests must have timeouts.
2. Authentication belongs inside the adapter.
3. Upstream IDs never become BSYSTEM canonical IDs.
4. Reads normalize into BSYSTEM views and Global IDs.
5. Writes require explicit capability and audit design before implementation.
6. Customer-facing data requires tenant/scope enforcement before exposure.
7. Contract tests must run without a live upstream system.
