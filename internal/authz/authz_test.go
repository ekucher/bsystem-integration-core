package authz

import (
	"context"
	"errors"
	"testing"
)

// fakeGrants is a deterministic scope-grant store keyed exactly as the
// database indexes them.
type fakeGrants struct {
	granted map[string]bool
	err     error
	calls   int
}

func (f *fakeGrants) HasScopedPermission(_ context.Context, principalType, principalID, scopeType, scopeID, permission string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.granted[key(principalType, principalID, scopeType, scopeID, permission)], nil
}

func key(parts ...string) string {
	result := ""
	for _, part := range parts {
		result += part + "|"
	}
	return result
}

// The platform's roles as migration 003 seeds them. The matrix below is
// asserted against these, so a change to the seeded permissions that widens
// access fails here.
var (
	administrator = Principal{ID: "USR-000001", Kind: KindUser, Roles: []string{"Administrator"}, Permissions: []string{"*"}}
	manager       = Principal{ID: "USR-000002", Kind: KindUser, Roles: []string{"Manager"}, Permissions: []string{"crm.client.read", "projects.task.read", "qa.report.read", "wiki.document.read", "operations.server.read", "support.incident.read"}}
	developer     = Principal{ID: "USR-000003", Kind: KindUser, Roles: []string{"Developer"}, Permissions: []string{"projects.task.read", "projects.task.edit", "development.repo.read", "development.pr.write", "qa.testcase.read", "wiki.document.read", "wiki.document.edit", "operations.server.read"}}
	qa            = Principal{ID: "USR-000004", Kind: KindUser, Roles: []string{"QA"}, Permissions: []string{"projects.task.read", "qa.testcase.read", "qa.testcase.execute", "qa.bug.write", "wiki.document.read"}}
	support       = Principal{ID: "USR-000005", Kind: KindUser, Roles: []string{"Support"}, Permissions: []string{"crm.client.read", "projects.task.read", "wiki.document.read", "operations.server.read", "support.incident.read", "support.incident.write"}}
	devops        = Principal{ID: "USR-000006", Kind: KindUser, Roles: []string{"DevOps"}, Permissions: []string{"development.repo.read", "wiki.document.read", "wiki.document.edit", "operations.server.read", "operations.server.manage"}}
	customer      = Principal{ID: "USR-000007", Kind: KindUser, Roles: []string{"Customer"}, Permissions: []string{"portal.read", "wiki.document.read", "support.incident.read"}}
	// A principal whose authentik groups map to no BSYSTEM role. There is no
	// "read only" role: an unmapped principal resolves to nothing at all.
	unmapped    = Principal{ID: "USR-000008", Kind: KindUser, Roles: nil, Permissions: nil}
	serviceCore = Principal{ID: "SVC-000001", Kind: KindService, Roles: []string{"Service Core"}, Permissions: []string{"adapters.read", "events.publish", "global_ids.read"}}
)

func newEvaluator(grants Grants) *Evaluator { return New(grants, DefaultConfinedRoles()) }

func allow(t *testing.T, e *Evaluator, principal Principal, permission string, scope Scope) Decision {
	t.Helper()
	decision, err := e.Evaluate(context.Background(), principal, permission, scope)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return decision
}

// TestAuthorizationMatrix is the role-by-resource matrix the platform's
// authorization guarantees rest on. Every cell is stated explicitly: an
// omission would silently become an allow the day a permission is widened.
func TestAuthorizationMatrix(t *testing.T) {
	evaluator := newEvaluator(&fakeGrants{})

	resources := map[string]string{
		"clients":       "crm.client.read",
		"contacts":      "crm.client.read",
		"projects":      "projects.task.read",
		"issues":        "projects.task.read",
		"documents":     "wiki.document.read",
		"rbac admin":    "*",
		"audit":         "*",
		"adapters":      "adapters.read",
		"events":        "events.publish",
		"incidents":     "support.incident.read",
		"server manage": "operations.server.manage",
	}

	// Expected allows per role. Everything not listed must be denied.
	matrix := map[string]struct {
		principal Principal
		allowed   []string
	}{
		"Administrator": {administrator, []string{"clients", "contacts", "projects", "issues", "documents", "rbac admin", "audit", "adapters", "events", "incidents", "server manage"}},
		"Manager":       {manager, []string{"clients", "contacts", "projects", "issues", "documents", "incidents"}},
		"Developer":     {developer, []string{"projects", "issues", "documents"}},
		"QA":            {qa, []string{"projects", "issues", "documents"}},
		"Support":       {support, []string{"clients", "contacts", "projects", "issues", "documents", "incidents"}},
		"DevOps":        {devops, []string{"documents", "server manage"}},
		// Customer is scope-confined: with no grant, every resource is denied,
		// including the ones its role permissions nominally cover.
		"Customer":     {customer, nil},
		"Read Only":    {unmapped, nil},
		"Service Core": {serviceCore, []string{"adapters", "events"}},
	}

	for roleName, row := range matrix {
		expected := map[string]bool{}
		for _, resource := range row.allowed {
			if _, ok := resources[resource]; !ok {
				t.Fatalf("matrix for %s names unknown resource %q", roleName, resource)
			}
			expected[resource] = true
		}
		for resource, permission := range resources {
			t.Run(roleName+"/"+resource, func(t *testing.T) {
				decision := allow(t, evaluator, row.principal, permission, Global())
				if decision.Allowed != expected[resource] {
					t.Fatalf("allowed = %v, want %v (reason: %s, code: %s)", decision.Allowed, expected[resource], decision.Reason, decision.Code)
				}
			})
		}
	}
}

// A confined role must be told that a scope is required, not that it lacks a
// permission it actually holds: the two denials mean different things to an
// administrator reading an audit trail.
func TestConfinedRoleDenialDistinguishesScopeFromPermission(t *testing.T) {
	evaluator := newEvaluator(&fakeGrants{})
	tests := []struct {
		name       string
		permission string
		wantCode   string
	}{
		{name: "holds the permission but no scope", permission: "wiki.document.read", wantCode: CodeScopeRequired},
		{name: "does not hold the permission", permission: "crm.client.read", wantCode: CodePermissionRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := allow(t, evaluator, customer, test.permission, Global())
			if decision.Allowed {
				t.Fatal("must be denied")
			}
			if decision.Code != test.wantCode {
				t.Fatalf("code = %q, want %q", decision.Code, test.wantCode)
			}
		})
	}
}

// A grant naming the resource is what lets a confined role through, and only
// for that resource.
func TestConfinedRoleIsAllowedOnlyWhereGranted(t *testing.T) {
	grants := &fakeGrants{granted: map[string]bool{
		key(KindUser, customer.ID, ScopeClient, "CL-000001", "wiki.document.read"): true,
	}}
	evaluator := newEvaluator(grants)

	tests := []struct {
		name        string
		scope       Scope
		wantAllowed bool
	}{
		{name: "the granted resource", scope: Resource(ScopeClient, "CL-000001"), wantAllowed: true},
		{name: "a sibling resource", scope: Resource(ScopeClient, "CL-000002")},
		{name: "the same id in another scope type", scope: Resource(ScopeProject, "CL-000001")},
		{name: "the unscoped collection", scope: Global()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := allow(t, evaluator, customer, "wiki.document.read", test.scope)
			if decision.Allowed != test.wantAllowed {
				t.Fatalf("allowed = %v, want %v", decision.Allowed, test.wantAllowed)
			}
		})
	}
}

// A grant can also be issued for a whole scope type, or globally.
func TestWildcardAndGlobalGrants(t *testing.T) {
	tests := []struct {
		name  string
		grant string
	}{
		{name: "every client", grant: key(KindUser, customer.ID, ScopeClient, "*", "wiki.document.read")},
		{name: "globally", grant: key(KindUser, customer.ID, ScopeGlobal, "*", "wiki.document.read")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluator := newEvaluator(&fakeGrants{granted: map[string]bool{test.grant: true}})
			decision := allow(t, evaluator, customer, "wiki.document.read", Resource(ScopeClient, "CL-000009"))
			if !decision.Allowed {
				t.Fatalf("a %s grant must allow the resource (code: %s)", test.name, decision.Code)
			}
		})
	}
}

// A scope grant can also give a permission the principal's role does not
// carry at all, which is how narrow delegation works.
func TestScopeGrantCanSupplyAPermissionTheRoleLacks(t *testing.T) {
	grants := &fakeGrants{granted: map[string]bool{
		key(KindUser, developer.ID, ScopeClient, "CL-000001", "crm.client.read"): true,
	}}
	evaluator := newEvaluator(grants)

	if decision := allow(t, evaluator, developer, "crm.client.read", Resource(ScopeClient, "CL-000001")); !decision.Allowed {
		t.Fatal("a scope grant must supply a permission the role lacks")
	}
	if decision := allow(t, evaluator, developer, "crm.client.read", Resource(ScopeClient, "CL-000002")); decision.Allowed {
		t.Fatal("the grant must not extend to another resource")
	}
}

// The administrator wildcard is platform-wide and needs no grant lookup.
func TestAdministratorNeedsNoScopeLookup(t *testing.T) {
	grants := &fakeGrants{}
	evaluator := newEvaluator(grants)
	if decision := allow(t, evaluator, administrator, "anything.at.all", Resource(ScopeProject, "PR-000001")); !decision.Allowed {
		t.Fatal("the administrator wildcard must allow any permission")
	}
	if grants.calls != 0 {
		t.Fatalf("administrator caused %d scope lookups, want 0", grants.calls)
	}
}

// A route that requires no permission still requires authentication; reaching
// the evaluator means the caller is already authenticated.
func TestEmptyPermissionAllowsAnAuthenticatedCaller(t *testing.T) {
	if decision := allow(t, newEvaluator(&fakeGrants{}), unmapped, "", Global()); !decision.Allowed {
		t.Fatal("an empty permission must allow an authenticated caller")
	}
}

// An unauthenticated principal must never be allowed, whatever it claims.
func TestPrincipalWithoutAnIdentityIsDenied(t *testing.T) {
	anonymous := Principal{Kind: KindUser, Permissions: []string{"*"}}
	if decision := allow(t, newEvaluator(&fakeGrants{}), anonymous, "crm.client.read", Global()); decision.Allowed {
		t.Fatal("a principal with no Global ID must be denied even with a wildcard permission")
	}
}

// A failed scope lookup must surface as an error, never as a decision. An
// unavailable store has to fail closed.
func TestScopeLookupFailureIsNotAnAllow(t *testing.T) {
	evaluator := newEvaluator(&fakeGrants{err: errors.New("scope store unavailable")})
	decision, err := evaluator.Evaluate(context.Background(), customer, "wiki.document.read", Global())
	if err == nil {
		t.Fatal("a failed scope lookup must return an error")
	}
	if decision.Allowed {
		t.Fatal("a failed scope lookup must never allow")
	}
}

// With no grant store at all, everything that depends on a grant is denied
// rather than allowed.
func TestNilGrantsDeniesRatherThanAllows(t *testing.T) {
	evaluator := New(nil, DefaultConfinedRoles())
	if decision := allow(t, evaluator, customer, "wiki.document.read", Global()); decision.Allowed {
		t.Fatal("a missing grant store must deny")
	}
	if decision := allow(t, evaluator, developer, "projects.task.read", Global()); !decision.Allowed {
		t.Fatal("a role permission must still apply without a grant store")
	}
}

// An unknown principal kind cannot be looked up, so it must fail rather than
// fall through to an allow.
func TestUnknownPrincipalKindIsRejected(t *testing.T) {
	evaluator := newEvaluator(&fakeGrants{})
	stranger := Principal{ID: "USR-000099", Kind: "robot", Permissions: nil}
	if _, err := evaluator.Evaluate(context.Background(), stranger, "crm.client.read", Global()); err == nil {
		t.Fatal("an unknown principal kind must be rejected")
	}
}

func TestIsConfined(t *testing.T) {
	evaluator := newEvaluator(&fakeGrants{})
	tests := []struct {
		name      string
		principal Principal
		want      bool
	}{
		{name: "customer", principal: customer, want: true},
		{name: "developer", principal: developer},
		{name: "administrator", principal: administrator},
		{name: "no roles", principal: unmapped},
		{name: "customer among others", principal: Principal{ID: "USR-000010", Kind: KindUser, Roles: []string{"Developer", "Customer"}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := evaluator.IsConfined(test.principal); got != test.want {
				t.Fatalf("IsConfined() = %v, want %v", got, test.want)
			}
		})
	}
}

// Denial reasons reach the caller, so they must name the permission and
// nothing else — no resource id, no owner, no upstream detail.
func TestDenialReasonsDiscloseNothingBeyondThePermission(t *testing.T) {
	evaluator := newEvaluator(&fakeGrants{})
	decision := allow(t, evaluator, developer, "crm.client.read", Resource(ScopeClient, "CL-000042"))
	if decision.Allowed {
		t.Fatal("must be denied")
	}
	if got := decision.Reason; got != "crm.client.read permission required" {
		t.Fatalf("reason = %q, which discloses more than the missing permission", got)
	}
}
