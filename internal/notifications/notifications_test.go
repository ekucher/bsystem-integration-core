package notifications

import "testing"

func TestOnlyMappedEventsRaiseNotifications(t *testing.T) {
	// The default for an unknown event is to interrupt nobody. A platform
	// that notified on everything would train people to ignore it.
	for _, event := range []string{"client.updated", "task.completed", "document.updated", "identity.created",
		// Successes and routine maintenance are reported and stored, but
		// nobody is interrupted by them.
		"backup.succeeded", "selftest.succeeded", "maintenance.started", "maintenance.completed", "server.ok", ""} {
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
		{"test.failed", "qa.testcase.read", "error"},
		{"selftest.failed", "operations.server.read", "error"},
		{"server.error", "operations.server.read", "error"},
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

// rolePermissions mirrors the human role grants in migration 003.
//
// It is duplicated here rather than read from the database because this is a
// statement about the mapping table, which is compiled in: an audience has to
// be a permission some role actually holds, and that is knowable without a
// running platform.
var rolePermissions = map[string][]string{
	"Manager":   {"crm.client.read", "projects.task.read", "qa.report.read", "wiki.document.read", "operations.server.read", "support.incident.read"},
	"Developer": {"projects.task.read", "projects.task.edit", "development.repo.read", "development.pr.write", "qa.testcase.read", "wiki.document.read", "wiki.document.edit", "operations.server.read"},
	"QA":        {"projects.task.read", "qa.testcase.read", "qa.testcase.execute", "qa.bug.write", "wiki.document.read"},
	"Support":   {"crm.client.read", "projects.task.read", "wiki.document.read", "operations.server.read", "support.incident.read", "support.incident.write"},
	"DevOps":    {"development.repo.read", "wiki.document.read", "wiki.document.edit", "operations.server.read", "operations.server.manage"},
	// Customer is scope-confined and is deliberately excluded: its
	// permissions mean "inside my own scope", so holding one is not the same
	// as being in an audience.
}

// An audience must be a permission that some unconfined role actually holds.
//
// A permission RBAC merely defines is not enough: qa.report.read exists, but
// only the Manager role holds it, so test.failed addressed there would never
// have reached the QA team. Nothing would have failed — the notification
// would simply have been invisible to the people it was for, which is the
// kind of defect that is only noticed by its absence.
func TestEveryAudienceIsHeldBySomeRole(t *testing.T) {
	holders := map[string][]string{}
	for role, permissions := range rolePermissions {
		for _, permission := range permissions {
			holders[permission] = append(holders[permission], role)
		}
	}
	for event, mapping := range Mappings() {
		if mapping.Permission == "*" {
			t.Errorf("%s is addressed to the administrator wildcard, which would notify only administrators", event)
			continue
		}
		if len(holders[mapping.Permission]) == 0 {
			t.Errorf("%s is addressed to %q, which no unconfined role holds: nobody but an administrator would ever see it",
				event, mapping.Permission)
		}
	}
}

// The events each role would be interrupted by, stated explicitly. A change
// to the mapping table that silently moves an event away from the people who
// act on it should have to be written down here first.
func TestRolesReceiveTheEventsTheyActOn(t *testing.T) {
	cases := map[string][]string{
		"QA":        {"test.failed"},
		"Developer": {"build.failed", "release.created", "test.failed", "backup.failed", "server.offline", "server.error", "selftest.failed"},
		"DevOps":    {"backup.failed", "server.offline", "server.error", "selftest.failed", "build.failed", "release.created"},
		"Support":   {"incident.created", "backup.failed", "server.offline", "server.error"},
	}
	mappings := Mappings()
	for role, events := range cases {
		held := map[string]bool{}
		for _, permission := range rolePermissions[role] {
			held[permission] = true
		}
		for _, event := range events {
			mapping, mapped := mappings[event]
			if !mapped {
				t.Errorf("%s is not in the mapping table", event)
				continue
			}
			if !held[mapping.Permission] {
				t.Errorf("%s would not reach %s: it is addressed to %q, which that role does not hold",
					event, role, mapping.Permission)
			}
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
