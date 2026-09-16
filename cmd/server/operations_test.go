package main

import (
	"os"
	"strings"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/operations"
)

// A server owned by a customer is authorized against that client, because
// that is where a grant would naturally be written. A server with no owner
// falls back to itself so a grant can still name exactly one host.
//
// Getting this wrong in the other direction is the dangerous case: evaluating
// an owned server against itself would mean a customer granted access to
// their client could not see their own infrastructure, and an administrator
// would be asked to write a grant per machine — which is how a scope model
// stops being used.
func TestServerScopeFollowsOwnership(t *testing.T) {
	cases := []struct {
		name      string
		server    operations.Server
		wantType  string
		wantScope string
	}{
		{
			name:     "an owned server is scoped to its client",
			server:   operations.Server{ID: "SRV-000001", ClientID: "CL-000001"},
			wantType: authz.ScopeClient, wantScope: "CL-000001",
		},
		{
			name:     "an unowned server is scoped to itself",
			server:   operations.Server{ID: "SRV-000002"},
			wantType: authz.ScopeResource, wantScope: "SRV-000002",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scopeType, scopeID := serverScope(c.server)
			if scopeType != c.wantType || scopeID != c.wantScope {
				t.Errorf("scope = (%s,%s), want (%s,%s)", scopeType, scopeID, c.wantType, c.wantScope)
			}
		})
	}
}

// Every event the platform reports and treats as worth interrupting someone
// about must have a notification mapping, and every event it treats as
// routine must not. The two tables are maintained separately, so nothing else
// would notice them drifting apart.
func TestSeriousOperationsEventsNotifySomebody(t *testing.T) {
	// Stated explicitly rather than derived from severity: "which failures
	// wake somebody up" is a decision, and deriving it would let a severity
	// change silently reassign it.
	interrupts := map[string]bool{
		"backup.succeeded":      false,
		"backup.failed":         true,
		"selftest.succeeded":    false,
		"selftest.failed":       true,
		"maintenance.started":   false,
		"maintenance.completed": false,
		"server.ok":             false,
		"server.warning":        false,
		"server.error":          true,
		"server.offline":        true,
	}
	vocabulary := operations.Events()
	if len(interrupts) != len(vocabulary) {
		t.Fatalf("the vocabulary has %d events and this test names %d; a new event needs a decision about notifying here",
			len(vocabulary), len(interrupts))
	}
	for name := range vocabulary {
		if _, named := interrupts[name]; !named {
			t.Errorf("%s is reportable but this test does not say whether it interrupts anyone", name)
		}
	}
	for name, shouldNotify := range interrupts {
		mapping, mapped := notificationMappingFor(name)
		switch {
		case shouldNotify && !mapped:
			t.Errorf("%s is a failure people act on but raises no notification", name)
		case !shouldNotify && mapped:
			t.Errorf("%s is routine but interrupts somebody; a platform that notifies on everything trains people to ignore it", name)
		case shouldNotify && mapping.Permission != "operations.server.read":
			t.Errorf("%s notifies %q rather than the people who read server state", name, mapping.Permission)
		}
	}
}

// The refusal must happen before the lookup. If it did not, a caller without
// the permission could tell an existing server from a missing one by the
// status code and enumerate which Global IDs name infrastructure — a database
// they were refused access to, read one bit at a time.
func TestTheServerDetailEndpointRefusesBeforeItLooksAnythingUp(t *testing.T) {
	source, err := os.ReadFile("operations_business.go")
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	body := string(source)
	start := strings.Index(body, "func (a *app) getServer(")
	if start < 0 {
		t.Fatal("getServer is not in this file any more; this test needs updating")
	}
	handler := body[start:]
	if end := strings.Index(handler[1:], "\nfunc "); end >= 0 {
		handler = handler[:end+1]
	}
	authorize := strings.Index(handler, "authorizeResource")
	lookup := strings.Index(handler, "a.db.GetServer")
	switch {
	case authorize < 0:
		t.Fatal("getServer no longer refuses a caller who cannot hold the permission anywhere")
	case lookup < 0:
		t.Fatal("getServer no longer looks a server up; this test needs updating")
	case authorize > lookup:
		t.Error("getServer looks the server up before refusing, so a refused caller can tell an existing server from a missing one")
	}
}
