// Package authz evaluates BSYSTEM authorization decisions.
//
// Every decision is made here rather than in route handlers, so the rules are
// stated once, tested directly, and cannot drift between endpoints. The
// evaluator denies by default: a request is allowed only when a rule says so.
//
// Two sources of authority are combined:
//
//   - Role permissions, resolved from the principal's authentik groups. These
//     apply across the whole platform.
//   - Scope grants, which give a principal a permission inside one tenant,
//     client, project or resource.
//
// A role whose access must be confined to what it explicitly owns — the
// Customer role today — cannot rely on its role permissions alone. It needs a
// scope grant naming the resource, so an unmapped or unknown resource is
// refused instead of disclosed.
package authz

import (
	"context"
	"errors"
	"strings"
)

// Scope types, matching the principal_scopes.scope_type domain.
const (
	ScopeGlobal   = "global"
	ScopeTenant   = "tenant"
	ScopeClient   = "client"
	ScopeProject  = "project"
	ScopeResource = "resource"
)

// Principal kinds, matching the principal_scopes.principal_type domain.
const (
	KindUser    = "user"
	KindService = "service"
)

// PermissionAll is the administrator wildcard.
const PermissionAll = "*"

// Stable machine-readable denial codes.
const (
	// CodeScopeRequired means the principal holds the permission but may only
	// exercise it inside a scope it has been granted, and no grant matches.
	CodeScopeRequired = "scope_required"
	// CodePermissionRequired means the principal does not hold the permission.
	CodePermissionRequired = "permission_required"
)

// Scope is the boundary a request is evaluated within. A list endpoint spans
// the whole platform and uses Global.
type Scope struct {
	Type string
	ID   string
}

// Global is the scope of a request that is not confined to one resource.
func Global() Scope { return Scope{Type: ScopeGlobal, ID: "*"} }

// Resource returns the scope of one specific resource.
func Resource(scopeType, id string) Scope { return Scope{Type: scopeType, ID: id} }

// Principal is the authenticated caller and the access their groups resolved
// to.
type Principal struct {
	ID          string
	Kind        string
	Roles       []string
	Permissions []string
}

// Grants answers whether a principal holds a permission inside one scope.
// platformdb.DB satisfies it.
type Grants interface {
	HasScopedPermission(ctx context.Context, principalType, principalID, scopeType, scopeID, permission string) (bool, error)
}

// Decision is the outcome of an evaluation.
//
// Reason is safe to return to the caller: it names the missing permission and
// nothing else. It never reveals whether the resource exists, who owns it, or
// anything about the upstream system.
type Decision struct {
	Allowed bool
	Reason  string
	Code    string
}

// Evaluator applies the platform's authorization rules.
type Evaluator struct {
	grants Grants
	// confinedRoles are roles that may only act inside an explicitly granted
	// scope, whatever their role permissions say.
	confinedRoles map[string]bool
}

// DefaultConfinedRoles lists the roles that must justify every access with an
// explicit scope grant.
//
// Customer is confined because customer-to-tenant ownership is not yet
// authoritative in BSYSTEM. Until it is, a customer request cannot be shown to
// stay inside its own tenant, and an undecided ownership boundary must deny
// rather than disclose.
func DefaultConfinedRoles() []string { return []string{"Customer"} }

// New returns an evaluator. A nil Grants means no scope grants exist, which
// denies every request that depends on one rather than allowing it.
func New(grants Grants, confinedRoles []string) *Evaluator {
	confined := make(map[string]bool, len(confinedRoles))
	for _, role := range confinedRoles {
		if role = strings.TrimSpace(role); role != "" {
			confined[role] = true
		}
	}
	return &Evaluator{grants: grants, confinedRoles: confined}
}

// IsConfined reports whether any of the principal's roles is scope-confined.
func (e *Evaluator) IsConfined(principal Principal) bool {
	for _, role := range principal.Roles {
		if e.confinedRoles[role] {
			return true
		}
	}
	return false
}

// Evaluate decides whether principal may exercise permission within scope.
//
// An error means the decision could not be made — a scope lookup failed — and
// must be surfaced as a dependency failure, never as an allow.
func (e *Evaluator) Evaluate(ctx context.Context, principal Principal, permission string, scope Scope) (Decision, error) {
	permission = strings.TrimSpace(permission)
	if permission == "" {
		// The route requires authentication only. Reaching the evaluator at
		// all means the caller is already authenticated.
		return Decision{Allowed: true}, nil
	}
	if principal.ID == "" {
		return deny(CodePermissionRequired, permission), nil
	}

	// The administrator wildcard is platform-wide and is not scope-confined:
	// no confined role carries it.
	if hasPermission(principal.Permissions, PermissionAll) {
		return Decision{Allowed: true}, nil
	}

	if !hasPermission(principal.Permissions, permission) {
		// The role does not grant it, so only an explicit scope grant can.
		granted, err := e.scoped(ctx, principal, permission, scope)
		if err != nil {
			return Decision{}, err
		}
		if granted {
			return Decision{Allowed: true}, nil
		}
		return deny(CodePermissionRequired, permission), nil
	}

	if e.IsConfined(principal) {
		granted, err := e.scoped(ctx, principal, permission, scope)
		if err != nil {
			return Decision{}, err
		}
		if granted {
			return Decision{Allowed: true}, nil
		}
		return Decision{
			Allowed: false,
			Reason:  permission + " requires a scope grant for this resource",
			Code:    CodeScopeRequired,
		}, nil
	}

	return Decision{Allowed: true}, nil
}

// scoped looks for a grant covering the scope exactly, or covering the whole
// scope type through the "*" wildcard.
func (e *Evaluator) scoped(ctx context.Context, principal Principal, permission string, scope Scope) (bool, error) {
	if e.grants == nil {
		return false, nil
	}
	kind := principal.Kind
	if kind != KindUser && kind != KindService {
		return false, errors.New("authz: principal kind must be user or service")
	}
	if scope.Type == "" {
		return false, errors.New("authz: scope type is required")
	}

	candidates := []Scope{scope}
	if scope.ID != "*" {
		candidates = append(candidates, Scope{Type: scope.Type, ID: "*"})
	}
	// A global grant covers every narrower scope.
	if scope.Type != ScopeGlobal {
		candidates = append(candidates, Scope{Type: ScopeGlobal, ID: "*"})
	}

	for _, candidate := range candidates {
		granted, err := e.grants.HasScopedPermission(ctx, kind, principal.ID, candidate.Type, candidate.ID, permission)
		if err != nil {
			return false, err
		}
		if granted {
			return true, nil
		}
	}
	return false, nil
}

func deny(code, permission string) Decision {
	return Decision{Allowed: false, Reason: permission + " permission required", Code: code}
}

func hasPermission(permissions []string, wanted string) bool {
	for _, permission := range permissions {
		if permission == wanted {
			return true
		}
	}
	return false
}
