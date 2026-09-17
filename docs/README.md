# Integration Core documentation

## Start here

| Document | What it answers |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | what this service is and what it is not |
| [API.md](API.md) | the conventions behind the API |
| [openapi.yaml](openapi.yaml) | **the authoritative contract** |
| [DEVELOPMENT.md](DEVELOPMENT.md) | how to run, test and extend it |

## Platform concepts

| Document | What it answers |
| --- | --- |
| [AUTHORIZATION.md](AUTHORIZATION.md) | roles, permissions, scopes, and how a decision is made |
| [AUDIT.md](AUDIT.md) | which operations may succeed without a durable audit record, and what happens when one cannot be written |
| [GLOBAL-IDS.md](GLOBAL-IDS.md) | how records are identified across systems |
| [EVENTS.md](EVENTS.md) | what the platform publishes and what a consumer may assume |
| [ADAPTERS.md](ADAPTERS.md) | how upstream systems are reached |
| [OBSERVABILITY.md](OBSERVABILITY.md) | logs, metrics and correlation |

## Modules

| Document | What it answers |
| --- | --- |
| [NOTIFICATIONS.md](NOTIFICATIONS.md) | who a notification is addressed to, and why not a person |
| [SEARCH.md](SEARCH.md) | why every result is authorized individually |
| [OPERATIONS-MODULE.md](OPERATIONS-MODULE.md) | servers, reported events, and the BRAVO contract |
| [SUPPORT.md](SUPPORT.md) | incidents, the lifecycle, and why the SLA targets are not here |
| [AI-GATEWAY.md](AI-GATEWAY.md) | what the gateway refuses to do, and why |
| [AI-GATEWAY-CONTRACT.md](AI-GATEWAY-CONTRACT.md) | the architectural principle it implements |

## Decisions

Read these when a convention looks arbitrary. Each one records what it is
avoiding, which the code cannot.

| ADR | Decision |
| --- | --- |
| [ADR-005](adr/ADR-005-customer-isolation-boundary.md) | unknown customer ownership denies rather than discloses |
| [ADR-006](adr/ADR-006-upstream-resilience-policy.md) | retries, backoff and circuit breaking |
| [ADR-007](adr/ADR-007-pagination-convention.md) | one envelope, opaque cursors, several encodings |
| [ADR-008](adr/ADR-008-normalized-errors.md) | nothing upstream is forwarded; callers branch on `code` |
| [ADR-009](adr/ADR-009-search-provider-architecture.md) | the provider narrows, the platform decides |
| [ADR-010](adr/ADR-010-ai-routing-policy.md) | provider choice is configuration, never a prompt |

## Historical

`P0.2-PERSISTENCE-AUDIT.md`, `P0.3-SERVICE-IDENTITIES-ADAPTERS.md` and
`P0.4-PERSISTENT-RBAC-SCOPES.md` record the foundation work as it was done.
They are kept for the reasoning, not as current reference — where they disagree
with the documents above, the documents above are right.

`ROADMAP.md` is the service's own plan; `bsystem-deploy/TASKS.md` is the
platform backlog and takes precedence.

## Stage readiness (P17)

- [`MIGRATION-READINESS.md`](MIGRATION-READINESS.md) — every migration audited
  for order, additivity, locking and rollback, plus what CI proves about
  applying them to an empty database.
- [`adapters/ESPOCRM-STAGE.md`](adapters/ESPOCRM-STAGE.md),
  [`adapters/REDMINE-STAGE.md`](adapters/REDMINE-STAGE.md),
  [`adapters/OUTLINE-STAGE.md`](adapters/OUTLINE-STAGE.md) — what each adapter
  requires of a real upstream, and the version-sensitive assumptions to check
  during acceptance. None of them claims tested live compatibility.

`cmd/mapping-audit` is a read-only diagnostic that reports mapping health —
invalid Global ID prefixes, one upstream record mapped twice, references to
Global IDs that do not resolve, and records with no owning client. It writes
nothing and contacts no source system:

```bash
DATABASE_URL=... go run ./cmd/mapping-audit -format=json
```

It exits non-zero when it finds an error, so it can gate an acceptance.
