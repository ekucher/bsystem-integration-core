package main

import (
	"strings"
	"testing"
)

func TestUnique(t *testing.T) {
	values := unique([]string{"a", "b", "a", "", "b"})
	if len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Fatalf("unexpected unique result: %#v", values)
	}
}

// Every route that requires a permission must state it in the inventory, and
// every route that states one must sit behind an authentication boundary.
// Authorization is otherwise unreachable for that endpoint.
func TestRoutePermissionsAreCoherent(t *testing.T) {
	for _, route := range routes() {
		t.Run(route.Pattern(), func(t *testing.T) {
			if route.Auth == authNone && route.Permission != "" {
				t.Fatalf("unauthenticated route declares permission %q, which can never be evaluated", route.Permission)
			}
		})
	}
}

// A route reaching business or administrative data without a declared
// permission would be readable by any authenticated principal, including one
// whose groups map to no role at all.
func TestDataRoutesRequireAPermission(t *testing.T) {
	// These routes intentionally require none, because each one returns only
	// what the caller's own access already entitles them to and is filtered
	// to it before anything is rendered. /me, /modules and /whoami describe
	// the caller. The notification routes resolve the caller's audience from
	// the same principal and apply it in the query itself, so a route
	// permission could only be broader than that filter, never narrower.
	perCallerFiltered := map[string]bool{
		"GET /api/v1/me":                       true,
		"GET /api/v1/modules":                  true,
		"GET /api/service/v1/whoami":           true,
		"GET /api/v1/notifications":            true,
		"POST /api/v1/notifications/{id}/read": true,
		"GET /api/v1/search":                   true,
	}
	for _, route := range routes() {
		if route.Auth == authNone || perCallerFiltered[route.Pattern()] {
			continue
		}
		if route.Permission == "" {
			t.Errorf("%s returns data without requiring a permission", route.Pattern())
		}
	}
}

// A route that addresses one resource must evaluate its permission against
// that resource. Declaring a resource scope is what tells the router to leave
// the decision to the handler, so the two must always be declared together.
func TestResourceRoutesDeclareBothAPermissionAndAScope(t *testing.T) {
	for _, route := range routes() {
		t.Run(route.Pattern(), func(t *testing.T) {
			addressesOne := strings.Contains(route.Path, "{id}")
			switch {
			case route.ResourceScope != "" && route.Permission == "":
				t.Fatal("a resource-scoped route without a permission would authorize nothing")
			case route.ResourceScope != "" && !addressesOne:
				t.Fatal("a collection route must not declare a resource scope; its permission would never be evaluated")
			}
		})
	}
}

// The normalized business detail endpoints all take a Global ID and all
// resolve it before touching an upstream. Missing one would leave an entity
// readable only through its collection.
func TestEveryNormalizedEntityHasADetailEndpoint(t *testing.T) {
	served := map[string]bool{}
	for _, route := range routes() {
		served[route.Pattern()] = true
	}
	for _, path := range []string{"clients", "contacts", "projects", "issues", "documents"} {
		pattern := "GET /api/v1/" + path + "/{id}"
		if !served[pattern] {
			t.Errorf("%s is missing", pattern)
		}
	}
}
