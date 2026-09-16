package platformdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/operations"
)

func newServer(id, name string) operations.Server {
	return operations.Server{
		ID: id, Name: name, Environment: "stage",
		Source: "inventory", SourceID: "src-" + id,
	}
}

func upsert(t *testing.T, ctx context.Context, db *DB, server operations.Server) operations.Server {
	t.Helper()
	stored, err := db.UpsertServer(ctx, server)
	if err != nil {
		t.Fatalf("upsert server: %v", err)
	}
	return stored
}

func TestServerRoundTripsAndUpsertIsLastWriteWins(t *testing.T) {
	ctx, db := storeFixture(t)

	first := upsert(t, ctx, db, newServer("SRV-000001", "billing-api-01"))
	if first.Status != "unknown" {
		t.Fatalf("a registered server starts at unknown status, got %q", first.Status)
	}
	if first.LastEventAt != nil {
		t.Fatal("a server nothing has been reported about has no last event")
	}

	renamed := newServer("SRV-000001", "billing-api-01-renamed")
	renamed.Environment = "prod"
	renamed.ClientID = "CL-000001"
	second := upsert(t, ctx, db, renamed)

	if second.Name != "billing-api-01-renamed" || second.Environment != "prod" {
		t.Fatalf("upsert must be last-write-wins for reporter-owned fields: %+v", second)
	}
	if second.ClientID != "CL-000001" {
		t.Fatalf("the relation must be stored, got %q", second.ClientID)
	}

	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM servers`).Scan(&count); err != nil {
		t.Fatalf("count servers: %v", err)
	}
	if count != 1 {
		t.Fatalf("upsert must not create a second row, got %d", count)
	}
}

// Registration must not be able to declare a server healthy. Status is a
// consequence of reported events; a reporter that could set it directly could
// say a server is fine without ever saying anything happened.
func TestRegistrationCannotSetStatus(t *testing.T) {
	ctx, db := storeFixture(t)

	claiming := newServer("SRV-000002", "web-01")
	claiming.Status = "ok"
	stored := upsert(t, ctx, db, claiming)
	if stored.Status != "unknown" {
		t.Fatalf("a registration claiming status %q must not set it, got %q", claiming.Status, stored.Status)
	}

	reported, err := db.InsertOperationsEvent(ctx, operations.Report{
		ServerID: "SRV-000002", Event: "server.online", Severity: "info",
		Source: "agent", OccurredAt: time.Now().UTC(),
	}, "ok")
	if err != nil {
		t.Fatalf("report event: %v", err)
	}
	if reported.ID == 0 {
		t.Fatal("a stored event must carry its id")
	}

	after, err := db.GetServer(ctx, "SRV-000002")
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	if after.Status != "ok" {
		t.Fatalf("an event that says the server is up must move the status, got %q", after.Status)
	}

	// A later registration must not roll the status back.
	again := upsert(t, ctx, db, newServer("SRV-000002", "web-01"))
	if again.Status != "ok" {
		t.Fatalf("re-registering must not reset a status the events established, got %q", again.Status)
	}
}

// "Nothing has been heard from this server" is an operational fact in its own
// right, so every report moves last_event_at — including one that says nothing
// about whether the server is up.
func TestEveryReportMovesLastEventAtButOnlySomeMoveStatus(t *testing.T) {
	ctx, db := storeFixture(t)
	upsert(t, ctx, db, newServer("SRV-000003", "db-01"))

	if _, err := db.InsertOperationsEvent(ctx, operations.Report{
		ServerID: "SRV-000003", Event: "server.offline", Severity: "critical",
		Source: "agent", OccurredAt: time.Now().UTC().Add(-time.Hour),
	}, "offline"); err != nil {
		t.Fatalf("report offline: %v", err)
	}

	offline, err := db.GetServer(ctx, "SRV-000003")
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	if offline.Status != "offline" || offline.LastEventAt == nil {
		t.Fatalf("expected offline with a last event, got %+v", offline)
	}
	firstHeard := *offline.LastEventAt

	// A report carrying no status: backup.failed says something happened, but
	// nothing about whether the machine is up.
	later := time.Now().UTC()
	if _, err := db.InsertOperationsEvent(ctx, operations.Report{
		ServerID: "SRV-000003", Event: "backup.failed", Severity: "error",
		Source: "backup", OccurredAt: later,
	}, ""); err != nil {
		t.Fatalf("report backup failure: %v", err)
	}

	after, err := db.GetServer(ctx, "SRV-000003")
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	if after.Status != "offline" {
		t.Fatalf("an event with no status must leave the status alone, got %q", after.Status)
	}
	if after.LastEventAt == nil || !after.LastEventAt.After(firstHeard) {
		t.Fatal("every report must move last_event_at")
	}
}

// The event and the status it implies are written together. A stored event
// whose status was not applied leaves a dashboard disagreeing with its own
// history; an applied status with no event cannot be explained to whoever asks.
func TestAnEventForAnUnknownServerStoresNothing(t *testing.T) {
	ctx, db := storeFixture(t)

	_, err := db.InsertOperationsEvent(ctx, operations.Report{
		ServerID: "SRV-NOPE", Event: "server.offline", Severity: "critical",
		Source: "agent", OccurredAt: time.Now().UTC(),
	}, "offline")
	if err == nil {
		t.Fatal("an event naming an unknown server must be refused")
	}

	var events int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM operations_events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("the refused event must not be stored, found %d", events)
	}
}

func TestUnknownServerIsReportedAsNotFound(t *testing.T) {
	ctx, db := storeFixture(t)

	if _, err := db.GetServer(ctx, "SRV-NOPE"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("expected ErrServerNotFound, got %v", err)
	}
	// A blank id must not resolve to some arbitrary row.
	if _, err := db.GetServer(ctx, "   "); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("a blank Global ID must not resolve, got %v", err)
	}
}

func TestServerListingFiltersAndPagesWithoutRepeating(t *testing.T) {
	ctx, db := storeFixture(t)

	for i, spec := range []struct{ id, environment, status string }{
		{"SRV-000010", "stage", "ok"},
		{"SRV-000011", "stage", "offline"},
		{"SRV-000012", "prod", "ok"},
		{"SRV-000013", "prod", "warning"},
		{"SRV-000014", "dev", ""},
	} {
		server := newServer(spec.id, "host-"+spec.id)
		server.Environment = spec.environment
		upsert(t, ctx, db, server)
		if spec.status != "" {
			if _, err := db.InsertOperationsEvent(ctx, operations.Report{
				ServerID: spec.id, Event: "server.online", Severity: "info",
				Source: "agent", OccurredAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
			}, spec.status); err != nil {
				t.Fatalf("seed status: %v", err)
			}
		}
	}

	for name, probe := range map[string]struct {
		query ServerQuery
		want  int
	}{
		"by environment": {ServerQuery{Environment: "stage", Limit: 50}, 2},
		"by status":      {ServerQuery{Status: "ok", Limit: 50}, 2},
		"both":           {ServerQuery{Environment: "prod", Status: "ok", Limit: 50}, 1},
		"unfiltered":     {ServerQuery{Limit: 50}, 5},
	} {
		items, total, err := db.ListServers(ctx, probe.query)
		if err != nil {
			t.Fatalf("%s: list servers: %v", name, err)
		}
		if len(items) != probe.want || total != probe.want {
			t.Errorf("%s: got %d items and total %d, want %d", name, len(items), total, probe.want)
		}
	}

	seen := map[string]int{}
	after := ""
	for page := 0; page < 10; page++ {
		items, _, err := db.ListServers(ctx, ServerQuery{AfterID: after, Limit: 2})
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
		t.Fatalf("paging walked %d servers, expected 5", len(seen))
	}
	for id, times := range seen {
		if times != 1 {
			t.Fatalf("server %s appeared %d times", id, times)
		}
	}
}

func TestOperationsEventListingFiltersAndPagesNewestFirst(t *testing.T) {
	ctx, db := storeFixture(t)
	upsert(t, ctx, db, newServer("SRV-000020", "a"))
	upsert(t, ctx, db, newServer("SRV-000021", "b"))

	for i, spec := range []struct{ server, event string }{
		{"SRV-000020", "server.online"},
		{"SRV-000020", "backup.failed"},
		{"SRV-000021", "server.offline"},
		{"SRV-000021", "backup.failed"},
		{"SRV-000020", "maintenance.started"},
	} {
		if _, err := db.InsertOperationsEvent(ctx, operations.Report{
			ServerID: spec.server, Event: spec.event, Severity: "info",
			Source: "agent", OccurredAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
		}, ""); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	byServer, total, err := db.ListOperationsEvents(ctx, OperationsEventQuery{ServerID: "SRV-000020", Limit: 50})
	if err != nil {
		t.Fatalf("list by server: %v", err)
	}
	if len(byServer) != 3 || total != 3 {
		t.Fatalf("expected 3 events for the server, got %d (total %d)", len(byServer), total)
	}

	byEvent, total, err := db.ListOperationsEvents(ctx, OperationsEventQuery{Event: "backup.failed", Limit: 50})
	if err != nil {
		t.Fatalf("list by event: %v", err)
	}
	if len(byEvent) != 2 || total != 2 {
		t.Fatalf("expected 2 backup failures, got %d (total %d)", len(byEvent), total)
	}

	// Newest first, and paging by descending id walks each event once.
	page, _, err := db.ListOperationsEvents(ctx, OperationsEventQuery{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page) != 2 || page[0].ID <= page[1].ID {
		t.Fatalf("events must come newest first, got %+v", page)
	}

	seen := map[int64]int{}
	var after int64
	for i := 0; i < 10; i++ {
		items, _, err := db.ListOperationsEvents(ctx, OperationsEventQuery{AfterID: after, Limit: 2})
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
		t.Fatalf("paging walked %d events, expected 5", len(seen))
	}
}

// Deleting a server takes its events with it. Orphan events would otherwise
// accumulate referring to a Global ID nothing resolves.
func TestServerDeletionCascadesToItsEvents(t *testing.T) {
	ctx, db := storeFixture(t)
	upsert(t, ctx, db, newServer("SRV-000030", "temporary"))

	if _, err := db.InsertOperationsEvent(ctx, operations.Report{
		ServerID: "SRV-000030", Event: "server.online", Severity: "info",
		Source: "agent", OccurredAt: time.Now().UTC(),
	}, "ok"); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM servers WHERE global_id=$1`, "SRV-000030"); err != nil {
		t.Fatalf("delete server: %v", err)
	}

	var orphans int
	if err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM operations_events WHERE server_id=$1`, "SRV-000030").Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("events must not outlive their server, found %d", orphans)
	}
}

// The rollback that matters is the one after a write has already succeeded.
//
// TestAnEventForAnUnknownServerStoresNothing covers the first write failing,
// where there was nothing to undo: the foreign key refuses the insert and the
// transaction had written nothing. This is the other half — the event is
// stored, and then the status update fails.
//
// InsertOperationsEvent says in its own comment that the two belong together:
// "a stored event whose status was not applied leaves a dashboard disagreeing
// with its own history, and an applied status with no event behind it cannot
// be explained to whoever asks why". That is the property, and only a partial
// failure can break it.
//
// The status column carries a CHECK, so a value outside its vocabulary fails
// the UPDATE deterministically without needing the database to be broken. A
// caller cannot reach this — the handler normalises the status first — which
// is the point: the transaction boundary has to hold for reasons the caller
// never has to know about.
//
// What this proves, precisely: the two writes share one transaction. It does
// not prove the error handling around them, and cannot — PostgreSQL aborts a
// transaction at the first failed statement, so the event is discarded whether
// or not the code checks that error. Dropping the check leaves this test
// green. Moving the update to the pool, so the insert commits on its own, is
// what fails it:
//
//	the event outlived the transaction that failed: 1 row(s)
//
// That is the regression worth guarding here — somebody splitting the two
// writes apart — rather than a missing `if err != nil` the database already
// covers.
func TestAFailedStatusUpdateUndoesTheStoredEvent(t *testing.T) {
	ctx, db := storeFixture(t)
	upsert(t, ctx, db, newServer("SRV-000009", "web-09"))

	before, err := db.GetServer(ctx, "SRV-000009")
	if err != nil {
		t.Fatalf("get server: %v", err)
	}

	_, err = db.InsertOperationsEvent(ctx, operations.Report{
		ServerID: "SRV-000009", Event: "server.offline", Severity: "critical",
		Source: "agent", OccurredAt: time.Now().UTC(),
	}, "not-a-status")
	if err == nil {
		t.Fatal("a status outside the column's vocabulary must be refused")
	}

	// The event was inserted before the update failed. It must not survive.
	var events int
	if err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM operations_events WHERE server_id=$1`, "SRV-000009").Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("the event outlived the transaction that failed: %d row(s)", events)
	}

	// And the server is untouched — not left with a moved last_event_at from
	// a report that never landed.
	after, err := db.GetServer(ctx, "SRV-000009")
	if err != nil {
		t.Fatalf("get server after the failure: %v", err)
	}
	if after.Status != before.Status {
		t.Errorf("status = %q, was %q; a failed report must change nothing", after.Status, before.Status)
	}
	if (after.LastEventAt == nil) != (before.LastEventAt == nil) {
		t.Errorf("last_event_at moved on a report that failed")
	}
}
