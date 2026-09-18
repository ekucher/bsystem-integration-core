package main

import (
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/authentikadmin"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

func TestActiveAdminCountExcludesServiceIdentities(t *testing.T) {
	t.Parallel()

	users := []authentikadmin.User{
		{
			PK:       1,
			Username: "human-admin",
			IsActive: true,
			Type:     "internal",
			GroupsObj: []authentikadmin.Group{
				{PK: "admins", Name: "BSYSTEM-Admins"},
			},
		},
		{
			PK:       2,
			Username: "service-by-type",
			IsActive: true,
			Type:     "service_account",
			GroupsObj: []authentikadmin.Group{
				{PK: "admins", Name: "BSYSTEM-Admins"},
			},
		},
		{
			PK:       3,
			Username: "service-by-group",
			IsActive: true,
			Type:     "internal",
			GroupsObj: []authentikadmin.Group{
				{PK: "admins", Name: "BSYSTEM-Admins"},
				{PK: "services", Name: "BSYSTEM-Services"},
			},
		},
		{
			PK:       4,
			Username: "disabled-admin",
			IsActive: false,
			Type:     "internal",
			GroupsObj: []authentikadmin.Group{
				{PK: "admins", Name: "BSYSTEM-Admins"},
			},
		},
	}

	if got := activeAdminCount(users); got != 1 {
		t.Fatalf("activeAdminCount() = %d, want 1", got)
	}
}

func TestMergeAdminAccountsFiltersServicesAndAttachesGlobalID(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 18, 1, 0, 0, 0, time.UTC)
	users := []authentikadmin.User{
		{
			PK:       5,
			Username: "human.support",
			Name:     "Human Support",
			Email:    "support@example.invalid",
			IsActive: true,
			Type:     "internal",
			GroupsObj: []authentikadmin.Group{
				{PK: "support", Name: "BSYSTEM-Support"},
			},
		},
		{
			PK:       6,
			Username: "machine",
			Name:     "Machine",
			IsActive: true,
			Type:     "service_account",
			GroupsObj: []authentikadmin.Group{
				{PK: "support", Name: "BSYSTEM-Support"},
			},
		},
	}
	identities := []platformdb.IdentityView{
		{
			ID:          "USR-000006",
			Username:    "human.support",
			DisplayName: "Human Support",
			Email:       "support@example.invalid",
			Groups:      []string{"BSYSTEM-Support"},
			FirstSeenAt: now,
			LastSeenAt:  now,
		},
	}

	accounts := mergeAdminAccounts(users, identities)
	if len(accounts) != 1 {
		t.Fatalf("mergeAdminAccounts() returned %d accounts, want 1: %#v", len(accounts), accounts)
	}
	account := accounts[0]
	if account.GlobalID == nil || *account.GlobalID != "USR-000006" {
		t.Fatalf("unexpected Global ID: %#v", account.GlobalID)
	}
	if !account.Manageable || !account.PasswordManageable {
		t.Fatalf("human internal account should be manageable: %#v", account)
	}
	if len(account.Roles) != 1 || account.Roles[0] != "support" {
		t.Fatalf("unexpected roles: %#v", account.Roles)
	}
}

func TestNormalizeAccountEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "valid", raw: "user@example.com", want: "user@example.com", ok: true},
		{name: "trim", raw: "  user@example.com  ", want: "user@example.com", ok: true},
		{name: "empty", raw: "", ok: false},
		{name: "display name rejected", raw: "User <user@example.com>", ok: false},
		{name: "malformed", raw: "not-an-email", ok: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := normalizeAccountEmail(tt.raw)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("normalizeAccountEmail(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}
