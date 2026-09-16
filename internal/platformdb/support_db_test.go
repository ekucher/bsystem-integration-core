package platformdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/support"
)

func newRecord(id string) support.Record {
	return support.Record{
		ID: id, Kind: "incident", Title: "Checkout latency above threshold",
		Summary:  "Sustained p95 latency on the ordering path.",
		Severity: "high", Status: "new", ReportedBy: "USR-000007",
	}
}

func create(t *testing.T, ctx context.Context, db *DB, record support.Record) support.Record {
	t.Helper()
	stored, err := db.CreateSupportRecord(ctx, record)
	if err != nil {
		t.Fatalf("create support record: %v", err)
	}
	return stored
}

func stringPointer(value string) *string { return &value }

func TestSupportRecordRoundTripsWithItsRelations(t *testing.T) {
	ctx, db := storeFixture(t)

	record := newRecord("INC-000001")
	record.ClientID = "CL-000001"
	record.Relations = []support.Relation{
		{EntityType: "server", GlobalID: "SRV-000002"},
		{EntityType: "project", GlobalID: "PR-000003"},
	}
	stored := create(t, ctx, db, record)

	if stored.ID != record.ID || stored.ClientID != "CL-000001" {
		t.Fatalf("stored record does not match: %+v", stored)
	}
	if len(stored.Relations) != 2 {
		t.Fatalf("expected two relations, got %d", len(stored.Relations))
	}

	read, err := db.GetSupportRecord(ctx, record.ID)
	if err != nil {
		t.Fatalf("get support record: %v", err)
	}
	if len(read.Relations) != 2 {
		t.Fatalf("a re-read must carry the relations, got %d", len(read.Relations))
	}
	if read.AcknowledgedAt != nil || read.ResolvedAt != nil {
		t.Fatal("a new record has neither SLA moment recorded")
	}
}

// An absent client is absent, not an empty string pretending to be one. The
// column is NULL, which is what the ownership audit and every scope query
// depend on.
func TestSupportRecordWithoutAClientStoresNoOwner(t *testing.T) {
	ctx, db := storeFixture(t)

	stored := create(t, ctx, db, newRecord("INC-000002"))
	if stored.ClientID != "" {
		t.Fatalf("expected no client, got %q", stored.ClientID)
	}

	// The audit finds it by looking for a NULL, so prove the column is one.
	var ownerless int
	if err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM support_records WHERE global_id=$1 AND client_id IS NULL`,
		"INC-000002").Scan(&ownerless); err != nil {
		t.Fatalf("count ownerless: %v", err)
	}
	if ownerless != 1 {
		t.Fatal("an absent client must be stored as NULL, not as an empty string")
	}
}

func TestUnknownSupportRecordIsReportedAsNotFound(t *testing.T) {
	ctx, db := storeFixture(t)

	if _, err := db.GetSupportRecord(ctx, "INC-NOPE"); !errors.Is(err, ErrSupportRecordNotFound) {
		t.Fatalf("get: expected ErrSupportRecordNotFound, got %v", err)
	}
	if _, err := db.UpdateSupportRecord(ctx, "INC-NOPE", SupportUpdate{
		Status: stringPointer("acknowledged"),
	}); !errors.Is(err, ErrSupportRecordNotFound) {
		t.Fatalf("update: expected ErrSupportRecordNotFound, got %v", err)
	}
}

// The property the schema comment promises: an SLA moment is recorded the
// first time the record reaches it and never moved again. Reopening a resolved
// incident must not erase that it was once resolved on time — otherwise an SLA
// report is rewritten by whoever reopens something.
func TestSLAMomentsAreRecordedOnceAndNeverMoved(t *testing.T) {
	ctx, db := storeFixture(t)
	create(t, ctx, db, newRecord("INC-000003"))

	acknowledged, err := db.UpdateSupportRecord(ctx, "INC-000003", SupportUpdate{
		Status: stringPointer("acknowledged"), Acknowledge: true,
	})
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if acknowledged.AcknowledgedAt == nil {
		t.Fatal("acknowledging must record the moment")
	}
	firstAcknowledgement := *acknowledged.AcknowledgedAt

	resolved, err := db.UpdateSupportRecord(ctx, "INC-000003", SupportUpdate{
		Status: stringPointer("resolved"), Resolve: true,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ResolvedAt == nil {
		t.Fatal("resolving must record the moment")
	}
	firstResolution := *resolved.ResolvedAt

	// Reopen, then reach both states again.
	if _, err := db.UpdateSupportRecord(ctx, "INC-000003", SupportUpdate{
		Status: stringPointer("in_progress"),
	}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	again, err := db.UpdateSupportRecord(ctx, "INC-000003", SupportUpdate{
		Status: stringPointer("resolved"), Acknowledge: true, Resolve: true,
	})
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}

	if !again.AcknowledgedAt.Equal(firstAcknowledgement) {
		t.Fatalf("acknowledged_at moved: %s then %s", firstAcknowledgement, *again.AcknowledgedAt)
	}
	if !again.ResolvedAt.Equal(firstResolution) {
		t.Fatalf("resolved_at moved: %s then %s", firstResolution, *again.ResolvedAt)
	}
}

// A partial update changes what it names and nothing else. COALESCE over a nil
// pointer is what makes that true, and it is easy to break by adding a field.
func TestPartialUpdateLeavesUnnamedFieldsAlone(t *testing.T) {
	ctx, db := storeFixture(t)

	original := create(t, ctx, db, newRecord("INC-000004"))

	updated, err := db.UpdateSupportRecord(ctx, "INC-000004", SupportUpdate{
		Severity: stringPointer("critical"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Severity != "critical" {
		t.Fatalf("severity was not updated: %q", updated.Severity)
	}
	if updated.Title != original.Title || updated.Summary != original.Summary {
		t.Fatalf("an unnamed field changed: %+v", updated)
	}
	if updated.Status != original.Status {
		t.Fatalf("status changed without being named: %q", updated.Status)
	}
	if !updated.UpdatedAt.After(original.UpdatedAt) && !updated.UpdatedAt.Equal(original.UpdatedAt) {
		t.Fatal("updated_at went backwards")
	}
}

// Relations replace wholesale when supplied, and are left alone when not.
// Getting this wrong in either direction is silent: relations accumulate, or
// they vanish on an unrelated edit.
func TestRelationsReplaceOnlyWhenSupplied(t *testing.T) {
	ctx, db := storeFixture(t)

	record := newRecord("INC-000005")
	record.Relations = []support.Relation{{EntityType: "server", GlobalID: "SRV-000001"}}
	create(t, ctx, db, record)

	untouched, err := db.UpdateSupportRecord(ctx, "INC-000005", SupportUpdate{
		Title: stringPointer("renamed"),
	})
	if err != nil {
		t.Fatalf("update without relations: %v", err)
	}
	if len(untouched.Relations) != 1 {
		t.Fatalf("relations must survive an update that does not name them, got %d", len(untouched.Relations))
	}

	replaced, err := db.UpdateSupportRecord(ctx, "INC-000005", SupportUpdate{
		Relations: []support.Relation{{EntityType: "project", GlobalID: "PR-000009"}},
	})
	if err != nil {
		t.Fatalf("update with relations: %v", err)
	}
	if len(replaced.Relations) != 1 || replaced.Relations[0].GlobalID != "PR-000009" {
		t.Fatalf("relations must be replaced wholesale, got %+v", replaced.Relations)
	}

	// An explicitly empty set clears them, which is different from not
	// supplying the field at all.
	cleared, err := db.UpdateSupportRecord(ctx, "INC-000005", SupportUpdate{
		Relations: []support.Relation{},
	})
	if err != nil {
		t.Fatalf("clear relations: %v", err)
	}
	if len(cleared.Relations) != 0 {
		t.Fatalf("an empty set must clear the relations, got %+v", cleared.Relations)
	}
}

func TestSupportListingFiltersAndPagesWithoutRepeating(t *testing.T) {
	ctx, db := storeFixture(t)

	for _, spec := range []struct {
		id, kind, severity, status, client string
	}{
		{"INC-000010", "incident", "high", "new", "CL-000001"},
		{"INC-000011", "incident", "low", "resolved", "CL-000001"},
		{"INC-000012", "incident", "critical", "in_progress", "CL-000002"},
		{"INC-000013", "request", "low", "new", "CL-000002"},
		{"INC-000014", "request", "medium", "closed", ""},
	} {
		record := newRecord(spec.id)
		record.Kind, record.Severity, record.Status, record.ClientID = spec.kind, spec.severity, spec.status, spec.client
		create(t, ctx, db, record)
	}

	for name, probe := range map[string]struct {
		query SupportQuery
		want  int
	}{
		"by kind":     {SupportQuery{Kind: "request", Limit: 50}, 2},
		"by severity": {SupportQuery{Severity: "low", Limit: 50}, 2},
		"by status":   {SupportQuery{Status: "new", Limit: 50}, 2},
		"by client":   {SupportQuery{ClientID: "CL-000001", Limit: 50}, 2},
		"open only":   {SupportQuery{OpenOnly: true, Limit: 50}, 3},
		"unfiltered":  {SupportQuery{Limit: 50}, 5},
	} {
		items, total, err := db.ListSupportRecords(ctx, probe.query)
		if err != nil {
			t.Fatalf("%s: list: %v", name, err)
		}
		if len(items) != probe.want || total != probe.want {
			t.Errorf("%s: got %d items and total %d, want %d", name, len(items), total, probe.want)
		}
	}

	// Paging by Global ID must walk the collection exactly once.
	seen := map[string]int{}
	after := ""
	for page := 0; page < 10; page++ {
		items, _, err := db.ListSupportRecords(ctx, SupportQuery{AfterID: after, Limit: 2})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			seen[item.ID]++
			after = item.ID
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paging walked %d records, expected 5", len(seen))
	}
	for id, times := range seen {
		if times != 1 {
			t.Fatalf("record %s appeared %d times", id, times)
		}
	}
}

// A listing deliberately does not load relations: a list is read to find a
// record, and fetching every relation for every row makes the common case pay
// for the rare one. Asserting it keeps a well-meant "fix" from regressing the
// listing into N+1 queries.
func TestListingDoesNotLoadRelations(t *testing.T) {
	ctx, db := storeFixture(t)

	record := newRecord("INC-000020")
	record.Relations = []support.Relation{{EntityType: "server", GlobalID: "SRV-000001"}}
	create(t, ctx, db, record)

	items, _, err := db.ListSupportRecords(ctx, SupportQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one record, got %d", len(items))
	}
	if len(items[0].Relations) != 0 {
		t.Fatalf("a listing must not carry relations, got %+v", items[0].Relations)
	}
	if detail, err := db.GetSupportRecord(ctx, "INC-000020"); err != nil || len(detail.Relations) != 1 {
		t.Fatalf("the detail read must carry them (err=%v relations=%d)", err, len(detail.Relations))
	}
}

// No SLA policy is seeded, because the targets are an owner decision. An empty
// map is the correct answer and must not be an error: the platform reports an
// unset SLA rather than inventing one.
func TestSLAPoliciesAreEmptyUntilTheOwnerSuppliesThem(t *testing.T) {
	ctx, db := storeFixture(t)

	policies, err := db.SLAPolicies(ctx)
	if err != nil {
		t.Fatalf("read SLA policies: %v", err)
	}
	if len(policies) != 0 {
		t.Fatalf("no SLA target is defined yet, got %+v", policies)
	}
}
