package platformdb

import (
	"context"
	"testing"
)

// These tests cover the data every authorization decision is built on. The
// evaluator in internal/authz has its own unit tests, but they run against
// in-memory fakes: if the queries that feed it resolve the wrong roles, or a
// scope grant matches more than it should, the evaluator is deciding
// correctly about the wrong facts and every one of those tests still passes.

func grant(t *testing.T, ctx context.Context, db *DB, g ScopeGrant) {
	t.Helper()
	if err := db.AddScopeGrant(ctx, g); err != nil {
		t.Fatalf("add scope grant: %v", err)
	}
}

func allowed(t *testing.T, ctx context.Context, db *DB, principalType, principalID, scopeType, scopeID, permission string) bool {
	t.Helper()
	ok, err := db.HasScopedPermission(ctx, principalType, principalID, scopeType, scopeID, permission)
	if err != nil {
		t.Fatalf("has scoped permission: %v", err)
	}
	return ok
}

func has(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// Deny by default is a property of the data, not only of the evaluator: with
// no grant at all, nothing is permitted.
func TestWithNoGrantNothingIsPermitted(t *testing.T) {
	ctx, db := storeFixture(t)

	if allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000001", "crm.client.read") {
		t.Fatal("a principal with no grant must be denied")
	}
}

// The exact-match case, and every near miss around it. Each of these would be
// a silent over-grant if the predicate were one comparison looser.
func TestScopeGrantMatchesExactlyAndNothingAdjacent(t *testing.T) {
	ctx, db := storeFixture(t)
	grant(t, ctx, db, ScopeGrant{
		PrincipalType: "user", PrincipalID: "USR-000001",
		ScopeType: "client", ScopeID: "CL-000001", PermissionID: "crm.client.read",
	})

	if !allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000001", "crm.client.read") {
		t.Fatal("the granted combination must be permitted")
	}

	for name, probe := range map[string][5]string{
		"another principal":      {"user", "USR-000002", "client", "CL-000001", "crm.client.read"},
		"another principal kind": {"service", "USR-000001", "client", "CL-000001", "crm.client.read"},
		"another scope type":     {"user", "USR-000001", "project", "CL-000001", "crm.client.read"},
		"another scope id":       {"user", "USR-000001", "client", "CL-000002", "crm.client.read"},
		"another permission":     {"user", "USR-000001", "client", "CL-000001", "crm.client.write"},
	} {
		if allowed(t, ctx, db, probe[0], probe[1], probe[2], probe[3], probe[4]) {
			t.Errorf("%s must not be permitted by this grant", name)
		}
	}
}

// A wildcard permission within one scope: everything about that client, and
// nothing about any other.
func TestWildcardPermissionAppliesOnlyInsideItsScope(t *testing.T) {
	ctx, db := storeFixture(t)
	grant(t, ctx, db, ScopeGrant{
		PrincipalType: "user", PrincipalID: "USR-000001",
		ScopeType: "client", ScopeID: "CL-000001", PermissionID: "*",
	})

	for _, permission := range []string{"crm.client.read", "support.incident.read", "anything.at.all"} {
		if !allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000001", permission) {
			t.Errorf("a wildcard permission must cover %q inside its scope", permission)
		}
	}
	if allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000002", "crm.client.read") {
		t.Fatal("a wildcard permission must not escape its scope")
	}
	if allowed(t, ctx, db, "user", "USR-000001", "project", "CL-000001", "crm.client.read") {
		t.Fatal("a wildcard permission must not cross to another scope type")
	}
}

// A wildcard scope id: every client, but still only clients.
func TestWildcardScopeIDStaysInsideItsScopeType(t *testing.T) {
	ctx, db := storeFixture(t)
	grant(t, ctx, db, ScopeGrant{
		PrincipalType: "user", PrincipalID: "USR-000001",
		ScopeType: "client", ScopeID: "*", PermissionID: "crm.client.read",
	})

	for _, client := range []string{"CL-000001", "CL-999999"} {
		if !allowed(t, ctx, db, "user", "USR-000001", "client", client, "crm.client.read") {
			t.Errorf("a wildcard scope id must cover client %s", client)
		}
	}
	if allowed(t, ctx, db, "user", "USR-000001", "project", "PR-000001", "crm.client.read") {
		t.Fatal("a wildcard scope id must not cross to another scope type")
	}
	if allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000001", "support.incident.read") {
		t.Fatal("a wildcard scope id must not widen the permission")
	}
}

// The global grant is the one that does cross scope types, deliberately. It is
// worth pinning precisely, because it is the grant that would hide every
// isolation bug behind it if it were handed out by accident.
func TestGlobalGrantCoversEveryScopeTypeButStillRespectsThePermission(t *testing.T) {
	ctx, db := storeFixture(t)
	grant(t, ctx, db, ScopeGrant{
		PrincipalType: "user", PrincipalID: "USR-000001",
		ScopeType: "global", ScopeID: "*", PermissionID: "crm.client.read",
	})

	for _, scope := range [][2]string{{"client", "CL-000001"}, {"project", "PR-000001"}, {"resource", "DOC-000001"}} {
		if !allowed(t, ctx, db, "user", "USR-000001", scope[0], scope[1], "crm.client.read") {
			t.Errorf("a global grant must cover scope type %s", scope[0])
		}
	}
	if allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000001", "support.incident.write") {
		t.Fatal("a global grant must still be bounded by its permission")
	}
}

func TestGrantsAreIdempotentAndDeletionRemovesOnlyTheNamedGrant(t *testing.T) {
	ctx, db := storeFixture(t)

	first := ScopeGrant{
		PrincipalType: "user", PrincipalID: "USR-000001",
		ScopeType: "client", ScopeID: "CL-000001", PermissionID: "crm.client.read",
	}
	second := first
	second.ScopeID = "CL-000002"

	grant(t, ctx, db, first)
	// Twice is not an error: a retried call must not fail, and must not
	// produce a second row that a later delete would leave behind.
	grant(t, ctx, db, first)
	grant(t, ctx, db, second)

	grants, err := db.ListScopeGrants(ctx, "user", "USR-000001")
	if err != nil {
		t.Fatalf("list scope grants: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected two grants, got %d", len(grants))
	}

	if err := db.DeleteScopeGrant(ctx, first); err != nil {
		t.Fatalf("delete scope grant: %v", err)
	}
	if allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000001", "crm.client.read") {
		t.Fatal("the deleted grant must stop permitting")
	}
	if !allowed(t, ctx, db, "user", "USR-000001", "client", "CL-000002", "crm.client.read") {
		t.Fatal("deleting one grant must not remove another")
	}

	// Deleting something that is not there is not an error; a client retrying
	// a revocation must not be told the grant vanished.
	if err := db.DeleteScopeGrant(ctx, first); err != nil {
		t.Fatalf("deleting an absent grant must be accepted, got %v", err)
	}
}

// Grants are listed per principal. A listing that leaked another principal's
// grants would disclose who can reach what.
func TestScopeGrantListingIsScopedToOnePrincipal(t *testing.T) {
	ctx, db := storeFixture(t)

	grant(t, ctx, db, ScopeGrant{PrincipalType: "user", PrincipalID: "USR-000001",
		ScopeType: "client", ScopeID: "CL-000001", PermissionID: "crm.client.read"})
	grant(t, ctx, db, ScopeGrant{PrincipalType: "user", PrincipalID: "USR-000002",
		ScopeType: "client", ScopeID: "CL-000002", PermissionID: "crm.client.read"})
	grant(t, ctx, db, ScopeGrant{PrincipalType: "service", PrincipalID: "USR-000001",
		ScopeType: "client", ScopeID: "CL-000003", PermissionID: "crm.client.read"})

	grants, err := db.ListScopeGrants(ctx, "user", "USR-000001")
	if err != nil {
		t.Fatalf("list scope grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected one grant for this principal, got %d: %+v", len(grants), grants)
	}
	if grants[0].ScopeID != "CL-000001" {
		t.Fatalf("the wrong principal's grant was returned: %+v", grants[0])
	}
}

// Human and service identities are separate surfaces.
//
// Two vocabularies meet here, and they are not the same word for a person:
// roles.kind is "human", while principal_scopes.principal_type is "user" — the
// value internal/authz passes. Using one where the other belongs resolves to
// nothing rather than failing, so it denies quietly instead of erroring, and
// these tests pin both spellings so that the next reader does not have to
// discover the difference the way this one did. A group that resolves to
// a human role must not resolve to anything for a service token, and the other
// way round — this is the separation the machine API depends on.
func TestAccessResolutionSeparatesHumanAndServiceIdentities(t *testing.T) {
	ctx, db := storeFixture(t)

	human, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Developers"}, "human")
	if err != nil {
		t.Fatalf("resolve human: %v", err)
	}
	if len(human.Roles) == 0 || !has(human.Permissions, "projects.task.read") {
		t.Fatalf("a developer must resolve to a role and its permissions: %+v", human)
	}

	asService, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Developers"}, "service")
	if err != nil {
		t.Fatalf("resolve developer as service: %v", err)
	}
	if len(asService.Roles) != 0 || len(asService.Permissions) != 0 {
		t.Fatalf("a human group must resolve to nothing for a service token: %+v", asService)
	}

	service, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Services"}, "service")
	if err != nil {
		t.Fatalf("resolve service: %v", err)
	}
	if !has(service.Permissions, "events.publish") {
		t.Fatalf("a service identity must resolve to its permissions: %+v", service)
	}

	asHuman, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Services"}, "human")
	if err != nil {
		t.Fatalf("resolve services as human: %v", err)
	}
	if len(asHuman.Roles) != 0 {
		t.Fatalf("a service group must resolve to nothing for a human token: %+v", asHuman)
	}
}

// An unmapped principal resolves to nothing at all. This is the case the
// isolation matrix marks as denied everywhere, and it has to be produced by
// the data rather than by a special case somewhere above it.
func TestUnmappedAndEmptyGroupsResolveToNothing(t *testing.T) {
	ctx, db := storeFixture(t)

	for name, groups := range map[string][]string{
		"an unknown group": {"BSYSTEM-Sasquatch"},
		"no groups":        {},
		"a nil group list": nil,
		"a near miss":      {"bsystem-developers"}, // group names are matched case-sensitively
	} {
		profile, err := db.ResolveAccess(ctx, groups, "human")
		if err != nil {
			t.Fatalf("%s: resolve: %v", name, err)
		}
		if len(profile.Roles) != 0 || len(profile.Permissions) != 0 || len(profile.Modules) != 0 {
			t.Errorf("%s must resolve to nothing, got %+v", name, profile)
		}
	}
}

// Several groups union their permissions without duplicating them.
func TestMultipleGroupsUnionWithoutDuplicates(t *testing.T) {
	ctx, db := storeFixture(t)

	combined, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Developers", "BSYSTEM-QA"}, "human")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(combined.Roles) != 2 {
		t.Fatalf("expected both roles, got %+v", combined.Roles)
	}

	seen := map[string]int{}
	for _, permission := range combined.Permissions {
		seen[permission]++
	}
	for permission, times := range seen {
		if times != 1 {
			t.Errorf("permission %q appears %d times", permission, times)
		}
	}
	// Both roles hold projects.task.read, which is why this union is the one
	// worth checking.
	if !has(combined.Permissions, "projects.task.read") || !has(combined.Permissions, "qa.bug.write") {
		t.Fatalf("the union must carry both roles' permissions: %+v", combined.Permissions)
	}
}

// A disabled role grants nothing. Disabling is how a role is withdrawn without
// deleting the history attached to it, so it has to actually withdraw it.
func TestDisabledRoleGrantsNothing(t *testing.T) {
	ctx, db := storeFixture(t)

	before, err := db.ResolveAccess(ctx, []string{"BSYSTEM-QA"}, "human")
	if err != nil {
		t.Fatalf("resolve before: %v", err)
	}
	if len(before.Roles) == 0 {
		t.Fatal("the QA role must resolve before being disabled")
	}

	if _, err := db.pool.Exec(ctx, `UPDATE roles SET enabled=FALSE WHERE id='qa'`); err != nil {
		t.Fatalf("disable role: %v", err)
	}

	after, err := db.ResolveAccess(ctx, []string{"BSYSTEM-QA"}, "human")
	if err != nil {
		t.Fatalf("resolve after: %v", err)
	}
	if len(after.Roles) != 0 || len(after.Permissions) != 0 || len(after.Modules) != 0 {
		t.Fatalf("a disabled role must grant nothing, got %+v", after)
	}
}

// ListRoles feeds the administrative view. It was de-N+1'd into batch queries,
// so the risk is a role losing its permissions or modules in the regrouping.
func TestListRolesCarriesEachRolesOwnPermissionsAndModules(t *testing.T) {
	ctx, db := storeFixture(t)

	roles, err := db.ListRoles(ctx)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles) == 0 {
		t.Fatal("the seeded roles must be listed")
	}

	byID := map[string]RoleView{}
	for _, role := range roles {
		byID[role.ID] = role
	}

	qa, ok := byID["qa"]
	if !ok {
		t.Fatalf("the qa role must be listed, got %v", byID)
	}
	if !has(qa.Permissions, "qa.bug.write") {
		t.Fatalf("qa must carry its own permissions: %+v", qa.Permissions)
	}
	// Permissions belonging to another role must not be attached to this one,
	// which is exactly what a mis-grouped batch query produces.
	if has(qa.Permissions, "crm.client.read") {
		t.Fatalf("qa must not carry another role's permissions: %+v", qa.Permissions)
	}

	administrator, ok := byID["administrator"]
	if !ok {
		t.Fatal("the administrator role must be listed")
	}
	if !has(administrator.Permissions, "*") {
		t.Fatalf("the administrator holds the wildcard permission: %+v", administrator.Permissions)
	}
}
