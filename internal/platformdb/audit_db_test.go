package platformdb

import (
	"strings"
	"testing"
	"time"
)

// The audit trail is the platform's evidence layer. The tenant isolation
// matrix says an acceptance correlates each result with it through the request
// id — that is the difference between "it denied me" and a record of why. An
// audit store that silently drops a field, or loses the correlation id, makes
// the acceptance unprovable rather than failed, which is worse: it looks fine.

func TestAuditEventRoundTripsEveryFieldTheAcceptanceReliesOn(t *testing.T) {
	ctx, db := storeFixture(t)

	event := AuditEvent{
		Subject:      "authentik-subject-1",
		GlobalUserID: "USR-000001",
		Action:       "client.read.denied",
		ResourceType: "client",
		ResourceID:   "CL-000001",
		RequestID:    "req-abc-123",
		SourceIP:     "203.0.113.7",
		Metadata:     map[string]any{"permission": "crm.client.read", "reason": "scope_required"},
	}
	if err := db.InsertAudit(ctx, event); err != nil {
		t.Fatalf("insert audit: %v", err)
	}

	items, err := db.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one audit event, got %d", len(items))
	}
	stored := items[0]

	for name, pair := range map[string][2]string{
		"subject":       {stored.Subject, event.Subject},
		"global_user":   {stored.GlobalUserID, event.GlobalUserID},
		"action":        {stored.Action, event.Action},
		"resource_type": {stored.ResourceType, event.ResourceType},
		"resource_id":   {stored.ResourceID, event.ResourceID},
		"request_id":    {stored.RequestID, event.RequestID},
		"source_ip":     {stored.SourceIP, event.SourceIP},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s did not round-trip: %q, want %q", name, pair[0], pair[1])
		}
	}

	// The request id is what ties an audit row to the smoke report and to the
	// platform's logs. Losing it does not break a request; it breaks the
	// ability to explain one afterwards.
	if stored.RequestID == "" {
		t.Fatal("the correlation id must survive; without it an audit row cannot be tied to the request it describes")
	}
	if got, _ := stored.Metadata["reason"].(string); got != "scope_required" {
		t.Fatalf("metadata did not round-trip: %+v", stored.Metadata)
	}
	if stored.ID == 0 {
		t.Fatal("a stored audit event must carry its id")
	}
	if stored.OccurredAt.IsZero() {
		t.Fatal("an audit event with no time is not evidence of anything")
	}
	if age := time.Since(stored.OccurredAt); age > time.Minute || age < -time.Minute {
		t.Fatalf("occurred_at is not close to now: %s", stored.OccurredAt)
	}
}

// A denial is the event most worth auditing, and it is recorded for a resource
// the caller could not see. The row must therefore be writable with no
// identity resolved and no metadata at all, rather than requiring fields the
// denial path does not have.
func TestADenialIsAuditableWithTheLeastInformationAvailable(t *testing.T) {
	ctx, db := storeFixture(t)

	if err := db.InsertAudit(ctx, AuditEvent{Action: "client.read.denied"}); err != nil {
		t.Fatalf("insert minimal audit event: %v", err)
	}

	items, err := db.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one audit event, got %d", len(items))
	}
	if items[0].Action != "client.read.denied" {
		t.Fatalf("the action must survive: %+v", items[0])
	}
	// Absent fields come back empty rather than as nulls that a reader has to
	// special-case.
	if items[0].Subject != "" || items[0].RequestID != "" || items[0].SourceIP != "" {
		t.Fatalf("absent fields must be empty strings: %+v", items[0])
	}
}

// An audit row must record what happened, not the payload it happened to. A
// column that could hold an upstream record would turn the audit trail into a
// second copy of customer data, governed by nothing.
func TestAuditSchemaHasNoColumnForUpstreamContent(t *testing.T) {
	ctx, db := storeFixture(t)

	rows, err := db.pool.Query(ctx, `
SELECT column_name FROM information_schema.columns
WHERE table_name = 'audit_events' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns = append(columns, name)
		for _, word := range []string{"payload", "body", "response", "content", "document", "email", "phone"} {
			if strings.Contains(name, word) {
				t.Errorf("audit_events.%s looks like it could hold upstream content", name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("audit_events has no columns; the migration did not run")
	}
}

func TestAuditListingIsNewestFirstAndRespectsItsLimit(t *testing.T) {
	ctx, db := storeFixture(t)

	for _, action := range []string{"first", "second", "third", "fourth", "fifth"} {
		if err := db.InsertAudit(ctx, AuditEvent{Action: action, RequestID: "req-" + action}); err != nil {
			t.Fatalf("insert %s: %v", action, err)
		}
		// occurred_at defaults to now() and the listing orders by it, so
		// events written inside the same clock tick would order arbitrarily.
		time.Sleep(2 * time.Millisecond)
	}

	items, err := db.ListAudit(ctx, 2)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("the limit must bound the page, got %d", len(items))
	}
	if items[0].Action != "fifth" || items[1].Action != "fourth" {
		t.Fatalf("events must come newest first, got %q then %q", items[0].Action, items[1].Action)
	}

	all, err := db.ListAudit(ctx, 50)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected five events, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].OccurredAt.After(all[i-1].OccurredAt) {
			t.Fatalf("ordering is not newest-first at position %d", i)
		}
	}
}

// Audit rows outlive the things they describe. A deleted server's audit
// history must remain: an incident review asks what happened to a machine that
// is no longer there, and a cascade would delete exactly the evidence being
// looked for.
func TestAuditRowsSurviveTheResourceTheyDescribe(t *testing.T) {
	ctx, db := storeFixture(t)

	if _, err := db.pool.Exec(ctx, `
INSERT INTO global_entities (global_id, entity_type, source, source_id)
VALUES ('SRV-000001','server','inventory','srv-1')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := db.InsertAudit(ctx, AuditEvent{
		Action: "server.read", ResourceType: "server", ResourceID: "SRV-000001", RequestID: "req-1",
	}); err != nil {
		t.Fatalf("insert audit: %v", err)
	}

	if _, err := db.pool.Exec(ctx, `DELETE FROM global_entities WHERE global_id='SRV-000001'`); err != nil {
		t.Fatalf("delete entity: %v", err)
	}

	items, err := db.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(items) != 1 || items[0].ResourceID != "SRV-000001" {
		t.Fatalf("the audit row must outlive the resource, got %+v", items)
	}
}
