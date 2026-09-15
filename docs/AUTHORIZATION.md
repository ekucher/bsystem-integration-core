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
