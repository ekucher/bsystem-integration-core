package platformdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Global IDs are immutable and everything else refers to them, so two records
// sharing one is not a cosmetic problem: it makes every reference ambiguous
// and there is no way to repair it afterwards, because a Global ID is
// deliberately not derivable from any upstream value.

var globalIDPattern = regexp.MustCompile(`^[A-Z]+-\d{6}$`)

func TestTheSameUpstreamRecordAlwaysGetsTheSameGlobalID(t *testing.T) {
	ctx, db := storeFixture(t)

	first, err := db.CreateGlobalEntity(ctx, "client", "espocrm", "acc-1", "", map[string]any{"name": "first"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !globalIDPattern.MatchString(first.GlobalID) {
		t.Fatalf("unexpected Global ID shape: %q", first.GlobalID)
	}

	// Asking again returns the same identity rather than allocating a second
	// one. Metadata differing must not change that: the upstream record is the
	// same record.
	again, err := db.CreateGlobalEntity(ctx, "client", "espocrm", "acc-1", "", map[string]any{"name": "changed"})
	if err != nil {
		t.Fatalf("create again: %v", err)
	}
	if again.GlobalID != first.GlobalID {
		t.Fatalf("Global ID is not stable: %q then %q", first.GlobalID, again.GlobalID)
	}
	if !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("re-requesting an existing entity must not restamp it")
	}
}

// The same source id in a different source, or under a different type, is a
// different record. Collapsing them would merge two unrelated things under one
// identity — a Redmine project #7 is not an EspoCRM account #7.
func TestIdentityIsScopedBySourceAndType(t *testing.T) {
	ctx, db := storeFixture(t)

	client, err := db.CreateGlobalEntity(ctx, "client", "espocrm", "7", "", nil)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	project, err := db.CreateGlobalEntity(ctx, "project", "redmine", "7", "", nil)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if client.GlobalID == project.GlobalID {
		t.Fatalf("two different records share a Global ID: %q", client.GlobalID)
	}

	// The prefix follows the type, which is what lets a reader tell what an id
	// refers to without a lookup.
	for _, probe := range []struct {
		entity GlobalEntity
		prefix string
	}{
		{client, "CL-"}, {project, "PR-"},
	} {
		if got := probe.entity.GlobalID[:3]; got != probe.prefix {
			t.Errorf("expected prefix %s, got %q", probe.prefix, probe.entity.GlobalID)
		}
	}
}

func TestAnUnsupportedEntityTypeIsRefusedRatherThanInvented(t *testing.T) {
	ctx, db := storeFixture(t)

	if _, err := db.CreateGlobalEntity(ctx, "sasquatch", "espocrm", "1", "", nil); err == nil {
		t.Fatal("an entity type with no registered prefix must be refused")
	}

	// And the refusal must leave nothing behind: a counter bumped for a type
	// that was then rejected would leave a gap that looks like a lost record.
	var entities int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM global_entities`).Scan(&entities); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if entities != 0 {
		t.Fatalf("the refused allocation stored something: %d entities", entities)
	}
}

// The test that single-threaded coverage cannot give: concurrent allocation.
// A counter read and written outside one transaction hands the same value to
// two callers, and the result is two entities carrying one Global ID — a
// failure that never appears until the platform is under real load.
func TestConcurrentAllocationNeverIssuesTheSameGlobalIDTwice(t *testing.T) {
	ctx, db := storeFixture(t)

	const workers = 12
	results := make([]string, workers)
	errs := make([]error, workers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < workers; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait() // release everyone at once, to actually contend
			entity, err := db.CreateGlobalEntity(ctx, "client", "espocrm",
				fmt.Sprintf("acc-%02d", index), "", nil)
			results[index], errs[index] = entity.GlobalID, err
		}(i)
	}
	start.Done()
	done.Wait()

	seen := map[string]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		seen[results[i]]++
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("Global ID %s was issued %d times", id, times)
		}
	}
	if len(seen) != workers {
		t.Fatalf("expected %d distinct Global IDs, got %d", workers, len(seen))
	}
}

// Concurrent requests for the *same* upstream record must converge on one
// identity rather than racing to create two.
func TestConcurrentRequestsForOneRecordConvergeOnOneGlobalID(t *testing.T) {
	ctx, db := storeFixture(t)

	const workers = 8
	results := make([]string, workers)
	errs := make([]error, workers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < workers; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			entity, err := db.CreateGlobalEntity(ctx, "client", "espocrm", "contended", "", nil)
			results[index], errs[index] = entity.GlobalID, err
		}(i)
	}
	start.Done()
	done.Wait()

	// This test used to tolerate a loser being rejected by the unique
	// constraint, on the reasoning that the database refusing to create a
	// second identity is better than silently allocating one. Both halves of
	// that are true, and they were not the only two options.
	//
	// The third is what the platform does now: the constraint still refuses
	// the duplicate, and the loser reads the row the winner just committed.
	// Nothing is invented, nothing is lost, and every caller gets the id.
	//
	// Tolerating the failure here mattered more than it looked, because of
	// what the error became one layer up. CreateGlobalEntity returned the
	// driver error as-is and the handler answered it as HTTP 400 with
	// err.Error() in the body — a raw SQLSTATE 23505 naming the table, the
	// column tuple and the constraint, telling a caller their valid input was
	// invalid, with 400 being the status nobody retries. Measured before the
	// fix, eight racers: six got an id, two got that.
	issued := map[string]bool{}
	for i := range results {
		if errs[i] != nil {
			t.Errorf("racer %d failed: %v; losing the race to allocate an id that now exists is not an error to report", i, errs[i])
			continue
		}
		issued[results[i]] = true
	}
	if len(issued) > 1 {
		t.Fatalf("one upstream record produced %d different Global IDs: %v", len(issued), issued)
	}
	if len(issued) == 0 {
		t.Fatal("every concurrent request failed")
	}

	// Whatever happened during the race, exactly one row exists afterwards.
	var rows int
	if err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM global_entities WHERE source='espocrm' AND entity_type='client' AND source_id='contended'`).
		Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected exactly one mapping, found %d", rows)
	}
}

func TestBatchMappingReusesExistingIdentitiesAndAllocatesTheRest(t *testing.T) {
	ctx, db := storeFixture(t)

	existing, err := db.CreateGlobalEntity(ctx, "client", "espocrm", "acc-1", "", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	mapped, err := db.MapGlobalIDs(ctx, "client", "espocrm", []GlobalIDRequest{
		{SourceID: "acc-1"},
		{SourceID: "acc-2"},
		{SourceID: "acc-3"},
		// A duplicate in the request must not allocate twice.
		{SourceID: "acc-2"},
		// An empty source id names no record and must be ignored rather than
		// mapped to something.
		{SourceID: ""},
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}

	if len(mapped) != 3 {
		t.Fatalf("expected three mappings, got %d: %v", len(mapped), mapped)
	}
	if mapped["acc-1"] != existing.GlobalID {
		t.Fatalf("an existing identity must be reused: %q vs %q", mapped["acc-1"], existing.GlobalID)
	}
	if _, ok := mapped[""]; ok {
		t.Fatal("an empty source id must not be mapped")
	}
	distinct := map[string]bool{}
	for _, id := range mapped {
		if !globalIDPattern.MatchString(id) {
			t.Errorf("unexpected Global ID shape: %q", id)
		}
		distinct[id] = true
	}
	if len(distinct) != 3 {
		t.Fatalf("mappings must be distinct, got %v", mapped)
	}

	// Mapping again is stable: the same call returns the same ids and creates
	// nothing new.
	again, err := db.MapGlobalIDs(ctx, "client", "espocrm", []GlobalIDRequest{
		{SourceID: "acc-1"}, {SourceID: "acc-2"}, {SourceID: "acc-3"},
	})
	if err != nil {
		t.Fatalf("map again: %v", err)
	}
	for sourceID, id := range again {
		if mapped[sourceID] != id {
			t.Fatalf("mapping is not stable for %s: %q then %q", sourceID, mapped[sourceID], id)
		}
	}

	var total int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM global_entities`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 3 {
		t.Fatalf("re-mapping allocated new identities: %d rows", total)
	}
}

func TestEmptyBatchMapsNothingAndTouchesNothing(t *testing.T) {
	ctx, db := storeFixture(t)

	mapped, err := db.MapGlobalIDs(ctx, "client", "espocrm", nil)
	if err != nil {
		t.Fatalf("map empty: %v", err)
	}
	if len(mapped) != 0 {
		t.Fatalf("an empty batch must map nothing, got %v", mapped)
	}
}

func TestResolvingAnUnknownGlobalIDFails(t *testing.T) {
	ctx, db := storeFixture(t)

	if _, err := db.ResolveGlobalEntity(ctx, "CL-999999"); err == nil {
		t.Fatal("an unknown Global ID must not resolve")
	}
}

// An entity type with no registered prefix is the caller's mistake, and the
// only CreateGlobalEntity failure whose text is safe to hand back. It is a
// sentinel so the HTTP layer can tell it apart without matching message text.
func TestAnUnsupportedEntityTypeIsDistinguishableFromADatabaseFailure(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := Open(ctx, migrationDatabase(ctx, t, adminDSN))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	_, err = db.CreateGlobalEntity(ctx, "not-an-entity", "espocrm", "x-1", "", nil)
	if !errors.Is(err, ErrUnsupportedEntityType) {
		t.Fatalf("error = %v, want one matching ErrUnsupportedEntityType", err)
	}
	if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "constraint") {
		t.Errorf("the caller-facing error carries database internals: %q", err)
	}
}
