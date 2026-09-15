# Integration Core Roadmap

## P0 — Foundation

- service skeleton
- PostgreSQL schema baseline
- `/health`
- `/api/v1/modules`
- service registry
- global ID mapping model
- request/correlation IDs
- structured logging
- authentik service authentication
- CI pipeline

## P1 — First business adapters

- EspoCRM adapter
- Redmine adapter
- client/project mappings
- basic synchronization jobs
- webhook receiver baseline

## P2 — Engineering adapters

- QA adapter
- Development adapter
- Outline adapter
- Operations adapter
- notification routing
- search indexing

## P3 — Event platform

- NATS transport
- event contracts
- retries
- dead-letter handling
- replay strategy where safe
- integration observability

## P4 — AI data services

- permission-aware context retrieval
- redaction/sanitization pipeline
- semantic-search indexing
- AI audit metadata
- model-independent context contracts

## P5 — Automation

- controlled cross-system workflows
- policy-enforced actions
- approval gates for sensitive actions
- workflow audit trail
