package main

import "testing"

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
	// /me and /modules intentionally require none: both return only what the
	// caller's own access already is, filtered to it.
	selfDescribing := map[string]bool{
		"GET /api/v1/me":             true,
		"GET /api/v1/modules":        true,
		"GET /api/service/v1/whoami": true,
	}
	for _, route := range routes() {
		if route.Auth == authNone || selfDescribing[route.Pattern()] {
			continue
		}
		if route.Permission == "" {
			t.Errorf("%s returns data without requiring a permission", route.Pattern())
		}
	}
}
