package notifications

import "testing"

func TestOnlyMappedEventsRaiseNotifications(t *testing.T) {
	// The default for an unknown event is to interrupt nobody. A platform
	// that notified on everything would train people to ignore it.
	for _, event := range []string{"client.updated", "task.completed", "document.updated", "identity.created", ""} {
		if _, mapped := FromEvent(Event{Event: event, Source: "test"}); mapped {
			t.Errorf("%q raised a notification but is not in the mapping table", event)
		}
	}
}

func TestMappedEventsCarryTheirAudienceAndSeverity(t *testing.T) {
	cases := []struct {
		event      string
		permission string
		severity   string
	}{
		{"backup.failed", "operations.server.read", "critical"},
		{"server.offline", "operations.server.read", "critical"},
		{"build.failed", "development.repo.read", "error"},
		{"test.failed", "qa.report.read", "error"},
		{"incident.created", "support.incident.read", "error"},
		{"release.created", "development.repo.read", "info"},
	}
	for _, c := range cases {
		t.Run(c.event, func(t *testing.T) {
			n, mapped := FromEvent(Event{Event: c.event, Source: "test"})
			if !mapped {
				t.Fatal("event is not mapped")
			}
			if n.AudiencePermission != c.permission {
				t.Errorf("audience = %q, want %q", n.AudiencePermission, c.permission)
			}
			if n.Severity != c.severity {
				t.Errorf("severity = %q, want %q", n.Severity, c.severity)
			}
			if n.RecipientID != "" {
				t.Error("a mapped event must not name a recipient: the platform does not know one")
			}
			if n.Title == "" {
				t.Error("a notification without a title tells a reader nothing")
			}
		})
	}
}

// Every audience must be a permission RBAC actually defines. A notification
// addressed to a permission nobody can hold is invisible, and one addressed
// to a typo is invisible in a way nobody notices.
func TestEveryAudienceIsAKnownPermission(t *testing.T) {
	// The set defined in migration 003.
	known := map[string]bool{
		"*": true, "crm.client.read": true, "projects.task.read": true,
		"projects.task.edit": true, "qa.report.read": true, "qa.testcase.read": true,
		"qa.testcase.execute": true, "qa.bug.write": true, "development.repo.read": true,
		"development.pr.write": true, "wiki.document.read": true, "wiki.document.edit": true,
		"operations.server.read": true, "operations.server.manage": true,
		"support.incident.read": true, "support.incident.write": true, "portal.read": true,
		"adapters.read": true, "events.publish": true, "global_ids.read": true,
	}
	for event, mapping := range Mappings() {
		if !known[mapping.Permission] {
			t.Errorf("%s is addressed to %q, which RBAC does not define", event, mapping.Permission)
		}
		if mapping.Permission == "*" {
			t.Errorf("%s is addressed to the administrator wildcard, which would notify only administrators", event)
		}
	}
}

func TestPublishersCanEscalateSeverityButNotLowerIt(t *testing.T) {
	cases := []struct {
		name     string
		floor    string
		reported string
		want     string
	}{
		{"a quieter report is ignored", "critical", "info", "critical"},
		{"an equal report changes nothing", "error", "error", "error"},
		{"a louder report is taken", "info", "critical", "critical"},
		{"an absent report keeps the floor", "error", "", "error"},
		{"an unrecognised report is not trusted", "error", "catastrophic", "error"},
		{"case does not matter", "info", "CRITICAL", "critical"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EscalateTo(c.floor, c.reported); got != c.want {
				t.Errorf("EscalateTo(%q,%q) = %q, want %q", c.floor, c.reported, got, c.want)
			}
		})
	}
}

func TestDeepLinksOnlyPointAtPagesThatExist(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"CL-000001", "/clients/CL-000001"},
		{"CT-000009", "/contacts/CT-000009"},
		{"PR-000002", "/projects/PR-000002"},
		{"TSK-000031", "/issues/TSK-000031"},
		{"DOC-000004", "/documents/DOC-000004"},
		// No HUB page exists for these yet, so no link is offered. A user
		// follows a link; one that leads nowhere is worse than none.
		{"SRV-000004", ""},
		{"INC-000001", ""},
		{"BUG-000007", ""},
		{"REL-000002", ""},
		// Not a Global ID at all.
		{"", ""},
		{"nonsense", ""},
		{"cl-000001", ""},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := DeepLink(c.id); got != c.want {
				t.Errorf("DeepLink(%q) = %q, want %q", c.id, got, c.want)
			}
		})
	}
}

func TestCursorsRoundTrip(t *testing.T) {
	for _, id := range []int64{1, 42, 999999} {
		cursor := EncodeCursor(id)
		if cursor == "" {
			t.Fatalf("EncodeCursor(%d) returned nothing", id)
		}
		got, err := DecodeCursor(cursor)
		if err != nil {
			t.Fatalf("DecodeCursor(%q): %v", cursor, err)
		}
		if got != id {
			t.Errorf("round trip gave %d, want %d", got, id)
		}
	}
}

func TestCursorsThePlatformDidNotIssueAreRejected(t *testing.T) {
	// A forged cursor must not be reinterpreted as a position: silently
	// starting somewhere else returns the wrong window and the caller never
	// learns it happened.
	for _, cursor := range []string{"not-base64!", "bzoxMA", "bjow", "bjot NQ", "nonsense"} {
		if _, err := DecodeCursor(cursor); err == nil {
			t.Errorf("DecodeCursor(%q) was accepted", cursor)
		}
	}
	if id, err := DecodeCursor("  "); err != nil || id != 0 {
		t.Errorf("an empty cursor should start at the newest notification, got (%d,%v)", id, err)
	}
}

func TestLimitsAreClampedRatherThanRejected(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultLimit}, {-5, DefaultLimit}, {1, 1},
		{MaxLimit, MaxLimit}, {MaxLimit + 1, MaxLimit}, {100000, MaxLimit},
	}
	for _, c := range cases {
		if got := ClampLimit(c.in); got != c.want {
			t.Errorf("ClampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEventDetailIsCarriedIntoTheNotification(t *testing.T) {
	n, mapped := FromEvent(Event{
		Event: "incident.created", Source: "support", Severity: "critical",
		EntityID: "INC-000001", TenantID: "CL-000001", CorrelationID: "4f1c2a",
		OccurredAt: "2026-01-06T02:14:00Z", Body: "database unreachable",
	})
	if !mapped {
		t.Fatal("incident.created should be mapped")
	}
	if n.Severity != "critical" {
		t.Errorf("severity = %q, want the publisher's escalation to critical", n.Severity)
	}
	if n.Body != "database unreachable" || n.EntityID != "INC-000001" ||
		n.TenantID != "CL-000001" || n.CorrelationID != "4f1c2a" ||
		n.OccurredAt != "2026-01-06T02:14:00Z" {
		t.Errorf("event detail was not carried: %+v", n)
	}
}
