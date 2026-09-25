package platformdb

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) (*DB, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)
	return db, ctx
}

func mustGlobalEntity(t *testing.T, db *DB, ctx context.Context, entityType, sourceID string) string {
	t.Helper()
	entity, err := db.CreateGlobalEntity(ctx, entityType, "test-source-"+entityType, sourceID, "", nil)
	if err != nil {
		t.Fatalf("create global entity %s/%s: %v", entityType, sourceID, err)
	}
	return entity.GlobalID
}

func TestRequirementPrefixRegistered(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	entity, err := db.CreateGlobalEntity(ctx, "requirement", "test-source-requirement", "req-"+suffix, "", nil)
	if err != nil {
		t.Fatalf("create requirement global entity: %v", err)
	}
	if !strings.HasPrefix(entity.GlobalID, "REQ-") {
		t.Fatalf("expected REQ- prefix, got %q", entity.GlobalID)
	}
}

func TestCreateRelationshipIsIdempotent(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "idem-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "idem-tst-"+suffix)

	actor := RelationshipActor{UserGlobalID: "USR-000001"}
	first, created, err := db.CreateRelationship(ctx, test, "tests", req, actor)
	if err != nil {
		t.Fatalf("create relationship: %v", err)
	}
	if !created {
		t.Fatalf("expected first call to create a row")
	}

	second, createdAgain, err := db.CreateRelationship(ctx, test, "tests", req, actor)
	if err != nil {
		t.Fatalf("create relationship again: %v", err)
	}
	if createdAgain {
		t.Fatalf("second identical call must not create a new row")
	}
	if second.ID != first.ID {
		t.Fatalf("expected the same row: first=%d second=%d", first.ID, second.ID)
	}
}

func TestCreateRelationshipDedupesInverseSpelling(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "inv-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "inv-tst-"+suffix)

	actor := RelationshipActor{UserGlobalID: "USR-000001"}
	forward, created, err := db.CreateRelationship(ctx, test, "tests", req, actor)
	if err != nil {
		t.Fatalf("create forward relationship: %v", err)
	}
	if !created {
		t.Fatalf("expected forward call to create a row")
	}

	// Declaring it from the requirement's side using the inverse verb must
	// resolve to the exact same canonical row, not a second one.
	inverse, createdInverse, err := db.CreateRelationship(ctx, req, "tested-by", test, actor)
	if err != nil {
		t.Fatalf("create inverse relationship: %v", err)
	}
	if createdInverse {
		t.Fatalf("inverse spelling of an existing edge must not create a new row")
	}
	if inverse.ID != forward.ID {
		t.Fatalf("expected the same canonical row: forward=%d inverse=%d", forward.ID, inverse.ID)
	}
	if inverse.FromGlobalID != test || inverse.ToGlobalID != req || inverse.RelationType != "tests" {
		t.Fatalf("expected canonical forward storage, got from=%s type=%s to=%s", inverse.FromGlobalID, inverse.RelationType, inverse.ToGlobalID)
	}
}

func TestCreateRelationshipRejectsSelfEdge(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "self-req-"+suffix)

	_, _, err := db.CreateRelationship(ctx, req, "related-to", req, RelationshipActor{UserGlobalID: "USR-000001"})
	if !errors.Is(err, ErrSelfRelationship) {
		t.Fatalf("expected ErrSelfRelationship, got %v", err)
	}
}

func TestCreateRelationshipRequiresAnActor(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "noactor-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "noactor-tst-"+suffix)

	_, _, err := db.CreateRelationship(ctx, test, "tests", req, RelationshipActor{})
	if !errors.Is(err, ErrMissingActor) {
		t.Fatalf("expected ErrMissingActor, got %v", err)
	}
}

func TestCreateRelationshipRejectsUnknownGlobalID(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "unknown-req-"+suffix)

	_, _, err := db.CreateRelationship(ctx, "TST-999999999", "tests", req, RelationshipActor{UserGlobalID: "USR-000001"})
	if !errors.Is(err, ErrUnknownGlobalID) {
		t.Fatalf("expected ErrUnknownGlobalID, got %v", err)
	}
}

func TestCreateRelationshipRejectsInvalidRelationType(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "badtype-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "badtype-tst-"+suffix)

	_, _, err := db.CreateRelationship(ctx, test, "supersedes", req, RelationshipActor{UserGlobalID: "USR-000001"})
	if !errors.Is(err, ErrInvalidRelationType) {
		t.Fatalf("expected ErrInvalidRelationType, got %v", err)
	}
}

func TestCreateRelationshipRecordsDualActorProvenance(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "dual-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "dual-tst-"+suffix)

	actor := RelationshipActor{UserGlobalID: "USR-000042", ServiceGlobalID: "SVC-000007", RequestID: "req-dual-" + suffix}
	row, created, err := db.CreateRelationship(ctx, test, "tests", req, actor)
	if err != nil {
		t.Fatalf("create relationship: %v", err)
	}
	if !created {
		t.Fatalf("expected a new row")
	}
	if row.CreatedByUserID != "USR-000042" || row.CreatedByServiceID != "SVC-000007" {
		t.Fatalf("expected both actors recorded on the row, got user=%q service=%q", row.CreatedByUserID, row.CreatedByServiceID)
	}

	audits, err := db.ListAudit(ctx, 500)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var found *AuditEvent
	for i := range audits {
		if audits[i].RequestID == actor.RequestID && audits[i].Action == "relationship.created" {
			found = &audits[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a relationship.created audit event for request %s", actor.RequestID)
	}
	if found.GlobalUserID != "USR-000042" {
		t.Fatalf("expected audit GlobalUserID to carry the human actor, got %q", found.GlobalUserID)
	}
	if found.Metadata["created_by_service_id"] != "SVC-000007" {
		t.Fatalf("expected audit metadata to carry the service actor, got %v", found.Metadata["created_by_service_id"])
	}
}

func TestListRelationshipsPresentsInverseFromOtherSide(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "list-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "list-tst-"+suffix)

	actor := RelationshipActor{ServiceGlobalID: "SVC-000001"}
	if _, _, err := db.CreateRelationship(ctx, test, "tests", req, actor); err != nil {
		t.Fatalf("create relationship: %v", err)
	}

	fromTest, err := db.ListRelationships(ctx, test)
	if err != nil {
		t.Fatalf("list from test: %v", err)
	}
	if !containsRelationshipView(fromTest, req, "tests", "outgoing") {
		t.Fatalf("expected outgoing 'tests' view from %s, got %+v", test, fromTest)
	}

	fromReq, err := db.ListRelationships(ctx, req)
	if err != nil {
		t.Fatalf("list from requirement: %v", err)
	}
	if !containsRelationshipView(fromReq, test, "tested-by", "incoming") {
		t.Fatalf("expected incoming 'tested-by' view from %s, got %+v", req, fromReq)
	}
}

func TestLookupRelationshipDoesNotAllocateOrCreate(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "lookup-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "lookup-tst-"+suffix)

	_, found, err := db.LookupRelationship(ctx, test, "tests", req)
	if err != nil {
		t.Fatalf("lookup relationship: %v", err)
	}
	if found {
		t.Fatalf("expected no relationship to exist yet")
	}

	list, err := db.ListRelationships(ctx, test)
	if err != nil {
		t.Fatalf("list relationships: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("lookup must not have created anything, got %+v", list)
	}
}

func containsRelationshipView(views []RelationshipView, globalID, relationType, direction string) bool {
	for _, view := range views {
		if view.GlobalID == globalID && view.RelationType == relationType && view.Direction == direction {
			return true
		}
	}
	return false
}

// TestCreateRelationshipConcurrentDuplicatesConvergeOnOneRow asserts that
// racing callers asking for the *same* edge — some spelling it forward, some
// spelling it as the inverse — never produce two rows. Exactly one racer
// should observe created=true; every racer, winner or loser, must resolve to
// the identical row ID. This is the concurrency counterpart of
// TestCreateRelationshipIsIdempotent and TestCreateRelationshipDedupesInverseSpelling:
// those prove idempotency and inverse-dedup happen sequentially, this proves
// the UNIQUE constraint plus ON CONFLICT DO NOTHING/SELECT fallback in
// CreateRelationship also make it hold under real contention, not just when
// calls happen to be serialized by the test itself.
func TestCreateRelationshipConcurrentDuplicatesConvergeOnOneRow(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	req := mustGlobalEntity(t, db, ctx, "requirement", "race-req-"+suffix)
	test := mustGlobalEntity(t, db, ctx, "test_case", "race-tst-"+suffix)

	const workers = 12
	ids := make([]int64, workers)
	created := make([]bool, workers)
	errs := make([]error, workers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < workers; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait() // release every racer at once, to actually contend
			actor := RelationshipActor{UserGlobalID: "USR-000001"}
			var row Relationship
			var err error
			if index%2 == 0 {
				row, created[index], err = db.CreateRelationship(ctx, test, "tests", req, actor)
			} else {
				// Half the racers spell the identical edge as its inverse,
				// from the other endpoint, so the test also exercises that a
				// race between the two spellings still converges on one row.
				row, created[index], err = db.CreateRelationship(ctx, req, "tested-by", test, actor)
			}
			ids[index], errs[index] = row.ID, err
		}(i)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}

	winners := 0
	for _, wasCreated := range created {
		if wasCreated {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one racer to create the row, got %d", winners)
	}

	firstID := ids[0]
	for i, id := range ids {
		if id != firstID {
			t.Fatalf("racer %d resolved to a different row: %d != %d", i, id, firstID)
		}
	}

	list, err := db.ListRelationships(ctx, test)
	if err != nil {
		t.Fatalf("list relationships: %v", err)
	}
	count := 0
	for _, view := range list {
		if view.GlobalID == req && view.RelationType == "tests" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one canonical edge to survive the race, found %d", count)
	}
}

// TestRelationshipBetweenDifferentSourceSystems proves relationships compose
// across Global IDs whose entity types come from different upstream systems
// (here: a QA test_case and a Redmine-sourced task) without a database join
// across those systems — entity_relationships only ever references
// global_entities, never a source-specific table.
func TestRelationshipBetweenDifferentSourceSystems(t *testing.T) {
	db, ctx := openTestDB(t)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")

	issue, err := db.CreateGlobalEntity(ctx, "task", "redmine", "issue-"+suffix, "", nil)
	if err != nil {
		t.Fatalf("create redmine task entity: %v", err)
	}
	if !strings.HasPrefix(issue.GlobalID, "TSK-") {
		t.Fatalf("expected TSK- prefix for redmine task, got %q", issue.GlobalID)
	}
	testCase := mustGlobalEntity(t, db, ctx, "test_case", "qa-tc-"+suffix)

	actor := RelationshipActor{UserGlobalID: "USR-000001"}
	row, created, err := db.CreateRelationship(ctx, testCase, "validates", issue.GlobalID, actor)
	if err != nil {
		t.Fatalf("create cross-system relationship: %v", err)
	}
	if !created {
		t.Fatalf("expected a new row for a fresh cross-system edge")
	}
	if row.FromGlobalID != testCase || row.ToGlobalID != issue.GlobalID || row.RelationType != "validates" {
		t.Fatalf("unexpected stored edge: %+v", row)
	}

	// Reading from either endpoint must work from the relationship store
	// alone — no join into a Redmine- or QA-specific table is involved.
	fromIssue, err := db.ListRelationships(ctx, issue.GlobalID)
	if err != nil {
		t.Fatalf("list from issue: %v", err)
	}
	if !containsRelationshipView(fromIssue, testCase, "validated-by", "incoming") {
		t.Fatalf("expected incoming 'validated-by' view from %s, got %+v", issue.GlobalID, fromIssue)
	}
}

// TestInverseRelationVocabularyCoversAllPairs table-drives every canonical
// verb the migration allows, asserting both InverseRelation's static mapping
// and that ListRelationships actually presents the derived inverse label
// when queried from the other endpoint — not just that the lookup table
// agrees with itself.
func TestInverseRelationVocabularyCoversAllPairs(t *testing.T) {
	cases := []struct {
		forward string
		inverse string
	}{
		{"related-to", "related-to"},
		{"tests", "tested-by"},
		{"validates", "validated-by"},
		{"documents", "documented-by"},
		{"implements", "implemented-by"},
		{"depends-on", "required-by"},
		{"blocks", "blocked-by"},
		{"references", "referenced-by"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.forward, func(t *testing.T) {
			inverse, ok := InverseRelation(tc.forward)
			if !ok {
				t.Fatalf("InverseRelation(%q) reported unknown relation", tc.forward)
			}
			if inverse != tc.inverse {
				t.Fatalf("InverseRelation(%q) = %q, want %q", tc.forward, inverse, tc.inverse)
			}

			db, ctx := openTestDB(t)
			suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "") + "-" + tc.forward
			from := mustGlobalEntity(t, db, ctx, "requirement", "vocab-from-"+suffix)
			to := mustGlobalEntity(t, db, ctx, "requirement", "vocab-to-"+suffix)

			actor := RelationshipActor{UserGlobalID: "USR-000001"}
			if _, _, err := db.CreateRelationship(ctx, from, tc.forward, to, actor); err != nil {
				t.Fatalf("create %s relationship: %v", tc.forward, err)
			}

			fromViews, err := db.ListRelationships(ctx, from)
			if err != nil {
				t.Fatalf("list from origin: %v", err)
			}
			if !containsRelationshipView(fromViews, to, tc.forward, "outgoing") {
				t.Fatalf("expected outgoing %q view from %s, got %+v", tc.forward, from, fromViews)
			}

			toViews, err := db.ListRelationships(ctx, to)
			if err != nil {
				t.Fatalf("list from target: %v", err)
			}
			if !containsRelationshipView(toViews, from, tc.inverse, "incoming") {
				t.Fatalf("expected incoming %q view from %s, got %+v", tc.inverse, to, toViews)
			}
		})
	}
}
