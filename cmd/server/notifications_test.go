package main

import (
	"reflect"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
)

// The audience a caller reads with is the notification endpoints' whole
// authorization boundary: there is no route permission behind it, so a
// mistake here is a disclosure rather than a broken page.
func TestNotificationAudienceIsDerivedFromThePrincipal(t *testing.T) {
	a := &app{authz: authz.New(nil, authz.DefaultConfinedRoles())}

	cases := []struct {
		name            string
		access          meResponse
		wantAll         bool
		wantPermissions []string
	}{
		{
			name:    "an administrator reads every audience",
			access:  meResponse{ID: "USR-000001", Roles: []string{"Administrator"}, Permissions: []string{"*"}},
			wantAll: true,
		},
		{
			name:            "an ordinary role reads the audiences its permissions name",
			access:          meResponse{ID: "USR-000002", Roles: []string{"DevOps"}, Permissions: []string{"operations.server.read", "operations.server.manage"}},
			wantPermissions: []string{"operations.server.read", "operations.server.manage"},
		},
		{
			// A confined role's permissions describe what it may do inside
			// scopes it has been granted. Reading them as an audience would
			// hand a customer every platform-wide notification matching those
			// permissions — the tenant boundary leaking through notifications.
			name:            "a scope-confined role reads only what names it",
			access:          meResponse{ID: "USR-000003", Roles: []string{"Customer"}, Permissions: []string{"portal.read", "support.incident.read", "wiki.document.read"}},
			wantPermissions: nil,
		},
		{
			name:            "a role that resolved to nothing reads only what names it",
			access:          meResponse{ID: "USR-000004"},
			wantPermissions: nil,
		},
		{
			// Confinement wins over a permission list, in either role order.
			name:            "a confined role alongside another role is still confined",
			access:          meResponse{ID: "USR-000005", Roles: []string{"Support", "Customer"}, Permissions: []string{"support.incident.read"}},
			wantPermissions: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			query := a.notificationQueryFor(c.access)
			if query.GlobalUserID != c.access.ID {
				t.Errorf("reader = %q, want %q", query.GlobalUserID, c.access.ID)
			}
			if query.AllAudiences != c.wantAll {
				t.Errorf("AllAudiences = %v, want %v", query.AllAudiences, c.wantAll)
			}
			if !reflect.DeepEqual(query.AudiencePermissions, c.wantPermissions) {
				t.Errorf("AudiencePermissions = %v, want %v", query.AudiencePermissions, c.wantPermissions)
			}
		})
	}
}

// An unauthenticated principal should never reach these handlers, but if the
// wiring ever changed, an empty reader must match nothing rather than
// everything.
func TestAnEmptyReaderCarriesNoAudience(t *testing.T) {
	a := &app{authz: authz.New(nil, authz.DefaultConfinedRoles())}
	query := a.notificationQueryFor(meResponse{})
	if query.GlobalUserID != "" || query.AllAudiences || len(query.AudiencePermissions) != 0 {
		t.Errorf("an empty principal resolved to a readable audience: %+v", query)
	}
}

// Only the publisher's own message is carried into a notification body.
// Copying the whole payload would put arbitrary upstream data in front of
// everyone holding the audience permission.
func TestOnlyTheEventMessageBecomesTheBody(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want string
	}{
		{"a message is taken", map[string]any{"message": " disk full "}, "disk full"},
		{"no payload is no body", nil, ""},
		{"a payload without a message is no body", map[string]any{"job": "nightly", "exit_code": 1}, ""},
		{"a non-string message is not coerced", map[string]any{"message": 42}, ""},
		{
			"other fields are never carried",
			map[string]any{"message": "backup failed", "api_key": "secret", "host": "db-internal-1"},
			"backup failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := eventBody(c.data); got != c.want {
				t.Errorf("eventBody = %q, want %q", got, c.want)
			}
		})
	}
}
