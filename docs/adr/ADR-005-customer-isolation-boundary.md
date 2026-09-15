# ADR-005: Customer isolation boundary

Status: Accepted

Date: 2026-09-15

ADR-001 to ADR-004 are recorded in `bsystem-hub/docs/adr`, which currently
holds the platform ADR series. This ADR continues that numbering and lives in
the Integration Core because authorization is enforced here.

## Context

BSYSTEM invariant 7 requires strict customer isolation, and invariant 6
requires denying by default. A customer must only ever reach data belonging to
their own tenant.

The platform cannot yet decide which tenant a customer belongs to.
Customer-to-tenant ownership is an owner-authoritative business mapping that
has not been supplied, and `TASKS.md` keeps it blocked. `global_entities`
carries an optional `tenant_id`, but nothing populates it for the entities the
adapters normalize today.

Meanwhile the seeded Customer role carries `portal.read`,
`wiki.document.read` and `support.incident.read`. Those role permissions are
platform-wide: under the ordinary rule, holding `wiki.document.read` would let
a customer read every internal document in Outline, including runbooks and
other customers' material.

The first implementation handled this with a check inside the document list
handler: if the caller had the Customer role, refuse. That was correct in
effect but wrong in shape. It protected exactly one endpoint, it had to be
restated on the next one, and it was invisible to anyone reading the
authorization rules.

## Decision

A role may be marked **scope-confined**. A confined role's permissions no
longer grant anything on their own: every request from it must be justified by
an explicit scope grant naming the tenant, client, project or resource.
Without a matching grant the request is denied with `scope_required`.

The Customer role is confined. No other role is.

The rule lives in `internal/authz`, so it applies to every endpoint that
consults the evaluator — present and future — rather than to whichever
handlers remember to ask.

Denials distinguish the two cases. A caller that does not hold the permission
is refused with `permission_required`; a caller that holds it but has no
matching grant is refused with `scope_required`. Neither reveals whether the
resource exists.

## Consequences

Customers are denied everything until ownership is modelled. That is the
intended outcome: an undecided ownership boundary must deny rather than
disclose, and a customer-facing API that guesses at ownership is worse than no
customer-facing API.

Lifting the restriction does not require code changes to any handler. It
requires populating tenant ownership and issuing scope grants, at which point
the same evaluator starts allowing exactly what the grants describe.

The confined-role list is a deliberate, reviewable statement of which roles
cannot act on their permissions alone. Adding a role to it is a security
decision made in one place.

An access decision can now fail because the scope store is unavailable. That
failure is surfaced as a dependency error, never as an allow: the evaluator
fails closed.

## Alternatives considered

**Remove `wiki.document.read` from the Customer role.** This makes the denial
look like a missing permission, which is misleading: the permission is
appropriate for the role, and the reason for refusal is the missing ownership
model. It would also have to be undone, and re-reasoned about, once tenancy
exists.

**Filter results by tenant instead of denying.** With no populated
`tenant_id`, every record would filter out, so the observable behaviour is the
same while the code implies an isolation guarantee it cannot yet make. A
partial filter that silently passes untagged records through would be worse:
it would leak.

**Keep the per-handler Customer check.** It cannot scale past a handful of
endpoints, and each new endpoint is one omission away from a disclosure.
