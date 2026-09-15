package main

import "testing"

func TestHasPermissionWildcard(t *testing.T) {
	access := meResponse{Permissions: []string{"*"}}
	if !hasPermission(access, "anything.at.all") {
		t.Fatal("wildcard permission must allow arbitrary permission")
	}
}

func TestUnique(t *testing.T) {
	values := unique([]string{"a", "b", "a", "", "b"})
	if len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Fatalf("unexpected unique result: %#v", values)
	}
}
