package platformdb

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// plan returns the chosen query plan as text.
func plan(t *testing.T, ctx context.Context, db *DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.pool.Query(ctx, "EXPLAIN (COSTS OFF) "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return strings.Join(lines, "\n")
}

// 009_indexes.sql explains why each index was added, which is reasoning rather
// than evidence. This is the evidence: with enough rows for the planner to have
// a choice, does it actually use them?
//
// The distinction matters because an index that is never chosen costs a write
// on every insert and disk forever in exchange for nothing, and an index that
// stops being chosen — because a query was rewritten, or the index was dropped
// in a later migration — is invisible until somebody notices the platform is
// slow. Neither shows up in a correctness test: the answers stay right.
//
// The rows are seeded here rather than assumed, because on an empty table
// PostgreSQL prefers a sequential scan whatever indexes exist, so a plan taken
// against an empty database proves nothing at all.
func TestTheIndexesAddedForTheseQueriesAreActuallyUsed(t *testing.T) {
	ctx, db := storeFixture(t)

	// Enough that a sequential scan is the more expensive option. A few
	// hundred rows is plenty for the planner to prefer an index; the point is
	// having a choice, not measuring anything.
	const rows = 400
	for i := 0; i < rows; i++ {
		if _, err := db.pool.Exec(ctx, `
INSERT INTO audit_events (subject,global_user_id,action,resource_type,resource_id,request_id,source_ip)
VALUES ($1,$2,'plan.probe','probe',$3,$4,'127.0.0.1')`,
			fmt.Sprintf("subject-%03d", i%40),
			fmt.Sprintf("USR-%06d", i%40),
			fmt.Sprintf("probe-%03d", i),
			fmt.Sprintf("request-%03d", i)); err != nil {
			t.Fatalf("seed audit row %d: %v", i, err)
		}
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE audit_events`); err != nil {
		t.Fatalf("analyze audit_events: %v", err)
	}

	// idx_audit_events_actor exists because the platform's own question is
	// "what did USR-000004 do" — the subject is what authentik calls them, the
	// Global ID is what every other record references.
	actorPlan := plan(t, ctx, db,
		`SELECT id FROM audit_events WHERE global_user_id = $1 ORDER BY id DESC LIMIT 20`,
		"USR-000004")
	if !strings.Contains(actorPlan, "idx_audit_events_actor") {
		t.Errorf("the audit-by-actor query does not use idx_audit_events_actor.\nPlan:\n%s", actorPlan)
	}

	// idx_notification_reads_reader exists because "my unread notifications"
	// is a lookup by reader alone, which the primary key
	// (notification_id, global_user_id) cannot serve. It runs on every page
	// load of the HUB's badge.
	for i := 0; i < rows; i++ {
		var id int64
		if err := db.pool.QueryRow(ctx, `
INSERT INTO notifications (event,source,severity,title,audience_permission,occurred_at)
VALUES ('plan.probe','test','info',$1,'*',now()) RETURNING id`,
			fmt.Sprintf("probe-%03d", i)).Scan(&id); err != nil {
			t.Fatalf("seed notification %d: %v", i, err)
		}
		if _, err := db.pool.Exec(ctx, `
INSERT INTO notification_reads (notification_id, global_user_id) VALUES ($1,$2)
ON CONFLICT DO NOTHING`, id, fmt.Sprintf("USR-%06d", i%40)); err != nil {
			t.Fatalf("seed notification read %d: %v", i, err)
		}
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE notifications, notification_reads`); err != nil {
		t.Fatalf("analyze notification tables: %v", err)
	}

	readerPlan := plan(t, ctx, db,
		`SELECT notification_id FROM notification_reads WHERE global_user_id = $1`,
		"USR-000004")
	if !strings.Contains(readerPlan, "idx_notification_reads_reader") {
		t.Errorf("the reads-by-reader lookup does not use idx_notification_reads_reader.\nPlan:\n%s", readerPlan)
	}
}

// MapGlobalIDs reads every mapping in one query and falls back to a per-record
// path only for records the platform has never seen. A comment beside that
// fallback says it "runs once per record in the platform's lifetime, not once
// per request", which is the difference between a collection read costing one
// query and costing one per row.
//
// This counts the scans PostgreSQL performs on global_entities, because that is
// the only thing that distinguishes the two. An earlier version of this test
// watched the Global ID counter instead, and a mutation proved it worthless:
// disabling the batched read entirely left the counter untouched, because the
// per-record path re-reads before it allocates and finds the row. The counter
// detects re-allocation. It does not detect an N+1, which is what the comment
// promises is absent.
//
// pg_stat_statements is not loaded here or in CI, so the table statistics are
// what is available; pg_stat_force_next_flush makes them synchronous enough to
// assert on, which they are not by default.
func TestASecondReadOfTheSameRecordsIsOneQuery(t *testing.T) {
	ctx, db := storeFixture(t)

	requests := make([]GlobalIDRequest, 0, 25)
	for i := 0; i < 25; i++ {
		requests = append(requests, GlobalIDRequest{
			EntityType: "client",
			Source:     "espocrm",
			SourceID:   fmt.Sprintf("n-plus-one-%02d", i),
		})
	}

	scans := func() int64 {
		t.Helper()
		if _, err := db.pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
			t.Fatalf("flush statistics: %v", err)
		}
		var seq, idx int64
		if err := db.pool.QueryRow(ctx, `
SELECT COALESCE(seq_scan,0), COALESCE(idx_scan,0)
FROM pg_stat_user_tables WHERE relname='global_entities'`).Scan(&seq, &idx); err != nil {
			t.Fatalf("read table statistics: %v", err)
		}
		return seq + idx
	}

	// First sighting: allocation is expected, and is the slow path by design.
	first, err := db.MapGlobalIDs(ctx, "client", "espocrm", requests)
	if err != nil {
		t.Fatalf("first mapping: %v", err)
	}
	if len(first) != len(requests) {
		t.Fatalf("first mapping returned %d of %d records", len(first), len(requests))
	}

	// Second sighting: every record is already mapped, so one batched read
	// should answer the whole collection.
	before := scans()
	second, err := db.MapGlobalIDs(ctx, "client", "espocrm", requests)
	if err != nil {
		t.Fatalf("second mapping: %v", err)
	}
	used := scans() - before

	// One scan for the batched read. A couple of spare is tolerance for the
	// statistics view itself, not room for a per-record path: twenty-five
	// records going one at a time would be twenty-five or more.
	const tolerated = 3
	if used > tolerated {
		t.Errorf("reading %d already-mapped records took %d scans of global_entities; a batched read is one, and one scan per record is the N+1 this is here to catch",
			len(requests), used)
	}

	for sourceID, globalID := range first {
		if second[sourceID] != globalID {
			t.Errorf("record %s mapped to %q and then to %q", sourceID, globalID, second[sourceID])
		}
	}
}
