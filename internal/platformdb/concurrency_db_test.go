package platformdb

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// release runs body in workers goroutines, all released together so they
// actually contend rather than queueing behind each other's setup, and returns
// each one's error.
//
// The release is the point. A loop that starts a goroutine and lets it run
// immediately usually finishes it before the next one begins, so the test
// passes without two callers ever being inside the code at once — which is the
// only thing it was written to check.
func release(workers int, body func(index int) error) []error {
	errs := make([]error, workers)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < workers; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			errs[index] = body(index)
		}(i)
	}
	start.Done()
	done.Wait()
	return errs
}

// A human signing in for the first time is the case most likely to arrive
// twice at once: a browser opening two tabs, or a page that fetches the profile
// while the session is still being established.
//
// EnsureIdentity takes an advisory lock on the subject and a comment beside it
// says that serializes first-seen allocation. This is what makes that a
// property rather than a claim.
//
// The `created` flag matters as much as the Global ID. It is what tells the
// platform this principal is new, and anything that hangs off first sight — an
// audit entry, an event, a welcome — fires once per true. Two callers both
// told they created the identity would each do that work, and the second one
// would look exactly like a legitimate first sighting.
func TestConcurrentEnsureIdentityAllocatesOneUser(t *testing.T) {
	ctx, db := storeFixture(t)

	const workers = 12
	subject := "concurrent-subject"
	ids := make([]string, workers)
	created := make([]bool, workers)

	errs := release(workers, func(index int) error {
		id, isNew, err := db.EnsureIdentity(ctx, Identity{
			Subject:     subject,
			Email:       "concurrent@example.invalid",
			DisplayName: "Concurrent Caller",
			Username:    "concurrent",
			Groups:      []string{"BSYSTEM-Developers"},
		})
		ids[index], created[index] = id, isNew
		return err
	})

	firsts := 0
	distinct := map[string]int{}
	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", index, err)
		}
		if ids[index] == "" {
			t.Fatalf("worker %d got an empty Global ID", index)
		}
		distinct[ids[index]]++
		if created[index] {
			firsts++
		}
	}

	if len(distinct) != 1 {
		t.Fatalf("%d concurrent sign-ins produced %d distinct Global IDs: %v", workers, len(distinct), distinct)
	}
	for id := range distinct {
		if !strings.HasPrefix(id, "USR-") {
			t.Errorf("Global ID %q does not carry the user prefix", id)
		}
	}
	if firsts != 1 {
		t.Errorf("%d callers were told they created the identity; exactly one first sighting is the whole point", firsts)
	}
}

// The same property for a machine. Service identities are handed out to
// integrations that start several replicas at once, so simultaneous first
// sightings are the normal case rather than the unlucky one.
func TestConcurrentEnsureServiceIdentityAllocatesOneService(t *testing.T) {
	ctx, db := storeFixture(t)

	const workers = 12
	ids := make([]string, workers)
	created := make([]bool, workers)

	errs := release(workers, func(index int) error {
		id, isNew, err := db.EnsureServiceIdentity(ctx, ServiceIdentity{
			Subject:  "concurrent-service",
			Name:     "Concurrent Service",
			Username: "svc-concurrent",
			Groups:   []string{"BSYSTEM-Services"},
		})
		ids[index], created[index] = id, isNew
		return err
	})

	firsts := 0
	distinct := map[string]int{}
	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", index, err)
		}
		distinct[ids[index]]++
		if created[index] {
			firsts++
		}
	}

	if len(distinct) != 1 {
		t.Fatalf("%d concurrent registrations produced %d distinct Global IDs: %v", workers, len(distinct), distinct)
	}
	for id := range distinct {
		if !strings.HasPrefix(id, "SVC-") {
			t.Errorf("Global ID %q does not carry the service prefix", id)
		}
	}
	if firsts != 1 {
		t.Errorf("%d callers were told they created the service identity; exactly one is correct", firsts)
	}
}

// Granting the same scope twice must not produce two rows, and granting and
// revoking at once must not leave the grant in a state neither caller asked
// for.
//
// The first half is what an administration UI does when somebody double-clicks.
// The second is what happens when one administrator grants while another
// revokes: the platform cannot decide who is right, but it must end up in one
// of the two states they asked for rather than a third.
func TestConcurrentScopeGrantsConvergeOnOneRow(t *testing.T) {
	ctx, db := storeFixture(t)

	grant := ScopeGrant{
		PrincipalType: "user",
		PrincipalID:   "USR-000001",
		ScopeType:     "client",
		ScopeID:       "CL-000001",
		PermissionID:  "crm.client.read",
	}

	const workers = 12
	errs := release(workers, func(int) error { return db.AddScopeGrant(ctx, grant, testAudit("rbac.scope.granted")) })
	for index, err := range errs {
		if err != nil {
			t.Fatalf("granting worker %d: %v", index, err)
		}
	}

	grants, err := db.ListScopeGrants(ctx, grant.PrincipalType, grant.PrincipalID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	matching := 0
	for _, stored := range grants {
		if stored == grant {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("%d concurrent identical grants produced %d rows; a grant is a fact, not a count", workers, matching)
	}

	// Now grant and revoke at once. Either outcome is correct; a duplicate row
	// or an error is not.
	mixed := release(workers, func(index int) error {
		if index%2 == 0 {
			return db.AddScopeGrant(ctx, grant, testAudit("rbac.scope.granted"))
		}
		return db.DeleteScopeGrant(ctx, grant, testAudit("rbac.scope.revoked"))
	})
	for index, err := range mixed {
		if err != nil {
			t.Fatalf("mixed worker %d: %v", index, err)
		}
	}

	grants, err = db.ListScopeGrants(ctx, grant.PrincipalType, grant.PrincipalID)
	if err != nil {
		t.Fatalf("list grants after the mixed run: %v", err)
	}
	matching = 0
	for _, stored := range grants {
		if stored == grant {
			matching++
		}
	}
	if matching > 1 {
		t.Fatalf("granting and revoking at once left %d rows; the outcome may be granted or revoked, but never both", matching)
	}
}

// Audit is append-only and every write is somebody's evidence. Concurrent
// writes must all survive: an audit trail that drops an entry under load is
// worse than none, because it is trusted.
func TestConcurrentAuditWritesAllSurvive(t *testing.T) {
	ctx, db := storeFixture(t)

	const workers = 24
	errs := release(workers, func(index int) error {
		return db.InsertAudit(ctx, AuditEvent{
			Subject:      "concurrent-subject",
			GlobalUserID: "USR-000001",
			Action:       "concurrency.probe",
			ResourceType: "probe",
			ResourceID:   fmt.Sprintf("probe-%02d", index),
			RequestID:    fmt.Sprintf("request-%02d", index),
		})
	})
	for index, err := range errs {
		if err != nil {
			t.Fatalf("audit worker %d: %v", index, err)
		}
	}

	events, err := db.ListAudit(ctx, 500)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	seen := map[string]int{}
	for _, event := range events {
		if event.Action == "concurrency.probe" {
			seen[event.ResourceID]++
		}
	}
	if len(seen) != workers {
		t.Fatalf("%d concurrent audit writes left %d distinct entries; an audit trail that loses an entry is trusted anyway", workers, len(seen))
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("audit entry %s appears %d times", id, times)
		}
	}
}
