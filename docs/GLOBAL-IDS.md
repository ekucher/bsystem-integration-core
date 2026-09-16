# Global IDs

A Global ID is the platform's name for a record that lives somewhere else.

## Why they exist

BSYSTEM reads from four source systems that have never heard of each other. A
client is an account id in EspoCRM, a project is a numeric id in Redmine, a
document is a UUID in Outline. Referring to any of them across module
boundaries means choosing an identifier, and the two obvious choices are both
wrong:

- **The upstream key** couples every module to that system's schema. Migrating
  EspoCRM, or integrating a second CRM, then rewrites every reference the
  platform ever stored.
- **A name** is not an identifier. Companies rename, projects get retitled,
  people change surnames and email addresses. A reference that breaks when a
  label changes is a reference that will break.

So the platform allocates its own.

## Format

```text
PREFIX-NNNNNN
```

| Prefix | Entity | Prefix | Entity |
| --- | --- | --- | --- |
| `USR-` | User | `SRV-` | Server |
| `SVC-` | Service identity | `INC-` | Incident |
| `CL-` | Client | `TST-` | Test case |
| `CT-` | Contact | `BUG-` | Bug |
| `PR-` | Project | `DOC-` | Document |
| `TSK-` | Task | `REL-` | Release |
| `REP-` | Repository | `APP-` | Application |

The number is a counter per entity type, not a hash and not a UUID. It is
allocated inside the transaction that creates the mapping, so two concurrent
first sightings of the same upstream record cannot produce two identifiers.

## The rules

**Immutable.** A Global ID never changes and is never reused. Everything else
in this document follows from that.

**Never derived from anything mutable.** Not a name, not a hostname, not an
email address, not a label. The allocation is keyed by `(source, entity_type,
source_id)` — the upstream's own stable key — and nothing else.

**Allocated once, on first sighting.** `CreateGlobalEntity` is idempotent on
that key: re-registering the same upstream record returns the identifier it
already has rather than minting a second.

**Reads never allocate.** `LookupGlobalEntity` returns an existing mapping
without creating one. This is not an optimisation. Reporting a contact's owning
client must not mint an identifier as a side effect, because a read that
changes the platform's state is a read that cannot be repeated safely — and
because an identifier allocated by a read is an identifier nobody decided to
create. An unmapped reference is **omitted** rather than allocated, so a
missing mapping never looks like a relationship.

## What they are not

**Not a capability.** Knowing a Global ID grants nothing. Every endpoint that
takes one authorizes it, and a record the caller may not read is answered
exactly as one that does not exist — see `AUTHORIZATION.md`. Global IDs are
sequential and therefore guessable, which is precisely why no endpoint may
treat possession of one as evidence of anything.

**Not a foreign key into an upstream.** The mapping records `source` and
`source_id`; callers get the Global ID. A caller that wants the upstream key
can see it on a normalized entity, but the platform never accepts one as an
identifier.

**Not a tenant boundary.** `tenant_id` on the mapping records which customer a
record belongs to, and is what scope grants are written against. The identifier
itself carries no authority.

## Where they are allocated

| Entity | Allocated by |
| --- | --- |
| `USR-` | first authenticated sighting of an authentik subject |
| `SVC-` | first authenticated sighting of a service identity |
| `CL-`, `CT-`, `PR-`, `TSK-`, `DOC-` | first mapping of an upstream record |
| `SRV-` | a reporter registering a host |
| `INC-` | the platform, when a support record is raised in BSYSTEM |

The last row is the only case where BSYSTEM is itself the source system, and
the mapping records that: `source` is `bsystem`.
