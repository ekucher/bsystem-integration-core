# BSYSTEM AI Gateway Contract

## Status

Architecture contract only. No LLM provider is enabled by this document.

## Principle

AI is an authorized consumer of BSYSTEM data, not a bypass around authorization.

```text
User / Service
    ↓
authentik
    ↓
BSYSTEM authorization
    ↓
AI Gateway
    ↓
Integration Core
    ↓
Authorized context only
    ↓
Model Router
```

AI must not connect directly to EspoCRM, Redmine, Outline, QA or Operations databases.

## Required request context

Every AI request must carry or derive:

```text
actor Global ID
request ID
roles
permissions
scopes
tenant/client context when applicable
purpose/operation
```

## Data classes

Initial policy classes:

```text
PUBLIC
INTERNAL
CONFIDENTIAL
SECRET
CREDENTIAL
```

Rules:

- `CREDENTIAL` is never sent to an LLM.
- secrets/tokens/passwords must be redacted before context assembly.
- `SECRET` requires an explicitly permitted provider policy.
- customer data must preserve tenant isolation.
- context retrieval must use the same authorization rules as non-AI APIs.

## Model routing

The gateway must support provider-independent routing:

```text
local model
OpenAI / approved cloud provider
future providers
```

Provider selection is policy-driven, not controlled directly by arbitrary user prompts.

## Audit

At minimum persist:

```text
actor_id
request_id
operation
model/provider
source systems
Global IDs accessed
data classification
policy decision
result status
```

Do not persist raw secrets or complete prompts containing restricted data merely for convenience.

## Tool/agent execution

Read actions may be introduced before write actions. Any mutating AI tool requires:

1. a dedicated permission;
2. resource scope evaluation;
3. explicit action schema validation;
4. audit event;
5. idempotency/replay protection where applicable;
6. human confirmation for high-impact operations unless a separately approved automation policy exists.

## Initial API direction

Future endpoints may use:

```text
POST /api/v1/ai/query
POST /api/v1/ai/search
GET  /api/v1/ai/models
```

These endpoints must not be added until context filtering, provider policy, audit and rate limiting exist.

## RAG

RAG indexes are derived data and inherit source permissions. Search results must be authorization-filtered before document chunks are returned to the model.

A vector index must never become a cross-tenant data leak path.
