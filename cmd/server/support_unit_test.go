package main

import (
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/support"
)

// A record naming a customer is authorized against that client, which is
// where a grant is written. Getting this backwards would mean a customer
// granted access to their own client still could not see their own incidents,
// and an administrator would be asked to write a grant per incident.
func TestSupportScopeFollowsTheAffectedCustomer(t *testing.T) {
	scopeType, scopeID := supportScope(support.Record{ID: "INC-000001", ClientID: "CL-000001"})
	if scopeType != authz.ScopeClient || scopeID != "CL-000001" {
		t.Errorf("scope = (%s,%s), want the client", scopeType, scopeID)
	}
	scopeType, scopeID = supportScope(support.Record{ID: "INC-000002"})
	if scopeType != authz.ScopeResource || scopeID != "INC-000002" {
		t.Errorf("scope = (%s,%s), want the record itself", scopeType, scopeID)
	}
}

// Incident severity and event severity are different scales, so the mapping
// between them is a decision that should be written down rather than a shared
// constant that silently couples them.
func TestIncidentSeverityMapsOntoEventSeverity(t *testing.T) {
	cases := map[string]string{
		support.SeverityCritical: "critical",
		support.SeverityHigh:     "error",
		support.SeverityMedium:   "warning",
		support.SeverityLow:      "info",
	}
	for severity, want := range cases {
		if got := eventSeverityFor(severity); got != want {
			t.Errorf("eventSeverityFor(%q) = %q, want %q", severity, got, want)
		}
	}
	// Anything unrecognised must be the quietest value rather than the
	// loudest: an unknown severity is not evidence of an emergency.
	if got := eventSeverityFor("nonsense"); got != "info" {
		t.Errorf("an unknown severity mapped to %q", got)
	}
}

// Support events must reach people through the notification table, and
// resolution must not.
func TestSupportEventsNotifyOnCreationOnly(t *testing.T) {
	if _, mapped := notificationMappingFor("incident.created"); !mapped {
		t.Error("a new incident interrupts nobody")
	}
	for _, event := range []string{"incident.updated", "incident.resolved"} {
		if _, mapped := notificationMappingFor(event); mapped {
			t.Errorf("%s raises a notification; good news and routine edits do not need to interrupt anyone", event)
		}
	}
}

// The support detail endpoints must refuse before they read, like every other
// detail endpoint. Both orderings look identical to an authorized caller, so
// only the source order can be checked.
func TestSupportDetailRefusesBeforeItLooksAnythingUp(t *testing.T) {
	assertRefusesBeforeLookup(t, "support.go", "func (a *app) resolveSupportRecord(", "a.db.GetSupportRecord")
}
