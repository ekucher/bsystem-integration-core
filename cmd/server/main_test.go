package main

import "testing"

func TestResolveAccessDeveloper(t *testing.T) {
	info := userInfo{
		Sub:               "subject-1",
		Email:             "dev@example.test",
		PreferredUsername: "developer",
		Groups:            []string{"BSYSTEM-Developers", "ignored-group", "BSYSTEM-Developers"},
	}
	access := resolveAccess(info, "USR-000001")
	if access.ID != "USR-000001" {
		t.Fatalf("unexpected Global ID: %s", access.ID)
	}
	if !contains(access.Roles, "Developer") {
		t.Fatalf("Developer role missing: %#v", access.Roles)
	}
	if !contains(access.Modules, "development") || !contains(access.Modules, "qa") {
		t.Fatalf("expected developer modules, got %#v", access.Modules)
	}
	if !hasPermission(access, "development.pr.write") {
		t.Fatal("developer permission missing")
	}
}

func TestAdministratorWildcard(t *testing.T) {
	access := resolveAccess(userInfo{Sub: "admin", Groups: []string{"BSYSTEM-Admins"}}, "USR-000002")
	if !hasPermission(access, "anything.at.all") {
		t.Fatal("administrator wildcard permission must allow arbitrary permission")
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
