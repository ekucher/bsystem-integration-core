# Authorization

Authorization is decided in `internal/authz` and enforced from the route
inventory in `cmd/server/routes.go`. No handler makes its own access decision.

## Model

Two sources of authority combine:

| Source | Scope | Origin |
| --- | --- | --- |
| Role permissions | the whole platform | authentik groups → `group_role_mappings` → `role_permissions` |
| Scope grants | one tenant, client, project or resource | `principal_scopes` |

The evaluator denies by default. A request is allowed only when a rule says
so, and an evaluation that cannot be completed — an unavailable scope store —
is an error, never an allow.

## Roles and groups

Groups originate in authentik and arrive as an OIDC claim. The platform maps
them to roles, and roles to permissions. Nothing is read from the token except
the group names: what a group *means* is the platform's decision, recorded in
migrations, not the identity provider's.

| authentik group | Role | Permissions |
| --- | --- | --- |
| `BSYSTEM-Admins` | Administrator | `*` |
| `BSYSTEM-Managers` | Manager | `crm.client.read`, `projects.task.read`, `qa.report.read`, `wiki.document.read`, `operations.server.read`, `support.incident.read` |
| `BSYSTEM-Developers` | Developer | `projects.task.read`, `projects.task.edit`, `development.repo.read`, `development.pr.write`, `qa.testcase.read`, `wiki.document.read`, `wiki.document.edit`, `operations.server.read` |
| `BSYSTEM-QA` | QA | `projects.task.read`, `qa.testcase.read`, `qa.testcase.execute`, `qa.bug.write`, `wiki.document.read` |
| `BSYSTEM-Support` | Support | `crm.client.read`, `projects.task.read`, `wiki.document.read`, `operations.server.read`, `support.incident.read`, `support.incident.write` |
| `BSYSTEM-DevOps` | DevOps | `development.repo.read`, `wiki.document.read`, `wiki.document.edit`, `operations.server.read`, `operations.server.manage` |
| `BSYSTEM-Customers` | Customer | `portal.read`, `wiki.document.read`, `support.incident.read` — **scope-confined** |
| `BSYSTEM-Services` | Service Core | `adapters.read`, `events.publish`, `global_ids.read`, `notifications.publish`, `operations.report`, `search.index` |

Two permissions are held by **no role**: `ai.query`, because who may spend
money on a model and whose data may be put in front of one is an owner
decision; and `operations.server.manage` is held only by DevOps. An
administrator reaches everything through the wildcard.

Human and service roles are separate `kind`s and are resolved separately, so a
human token can never pick up a service permission by being in the wrong
group.

The table above is maintained in `internal/platformdb/migrations/003` and
later migrations. Where this document and a migration disagree, the migration
is what runs.

## Decision order

1. An empty required permission allows any authenticated caller. Only
   endpoints that return the caller's own access use this.
2. A principal with no Global ID is denied, whatever permissions it claims.
3. The `*` permission allows everything. It is the administrator wildcard and
   needs no scope lookup.
4. If the principal's roles do not carry the permission, a scope grant may
   still supply it for the specific resource. Otherwise the request is denied
   with `permission_required`.
5. If the principal's roles carry the permission and none of its roles is
   scope-confined, the request is allowed.
6. If any of its roles is scope-confined, the request needs a scope grant
   covering the resource. Otherwise it is denied with `scope_required`.

A grant matches the exact scope, the whole scope type through `*`, or the
global scope.

## Scope-confined roles

A confined role cannot act on its role permissions alone; every access must be
justified by an explicit grant. **Customer** is confined, because
customer-to-tenant ownership is not yet authoritative in BSYSTEM. See
[ADR-005](adr/ADR-005-customer-isolation-boundary.md).

## Enforcement

Each route declares the permission it requires:

```go
{Method: http.MethodGet, Path: "/api/v1/clients", Auth: authHuman, Permission: "crm.client.read", Handler: ...}
```

`authorize` evaluates it before the handler runs. A route that addresses one
resource evaluates again through `authorizeResource` once it has resolved
which resource was addressed, so a Global ID belonging to someone else is
refused rather than fetched.

Access is resolved once per request, in the authentication middleware, so a
single request cannot observe two different authorization states.

## Denials

| Code | Meaning |
| --- | --- |
| `permission_required` | the caller does not hold the permission |
| `scope_required` | the caller holds it, but no grant covers this resource |

Both return `403`. The body names the missing permission and nothing else: it
never discloses whether the resource exists, who owns it, or anything from the
upstream system.

## Testing

`internal/authz/authz_test.go` holds the role-by-resource matrix. Every cell is
stated explicitly — an omitted cell would silently become an allow the day a
role's permissions widen — along with the confined-role rules, grant matching,
fail-closed behaviour and the guarantee that denial reasons disclose nothing
beyond the permission.

The end-to-end authorization and IDOR scenarios live in
`bsystem-deploy/e2e`, which exercises the same rules through the HTTP surface
against the mock stack.
