package platformdb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// decodeEnvelope reads the queued payload the way a consumer would.
func decodeEnvelope(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode queued envelope: %v (payload: %s)", err, payload)
	}
	return envelope
}

// queuedFor returns the outbox rows whose envelope names this entity.
func queuedFor(t *testing.T, ctx context.Context, db *DB, entityID string) []OutboxEvent {
	t.Helper()
	rows, err := db.pool.Query(ctx, `
SELECT event_id, subject, payload, occurred_at, attempts
FROM event_outbox WHERE payload->>'entity_id' = $1 ORDER BY occurred_at`, entityID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	found := []OutboxEvent{}
	for rows.Next() {
		var event OutboxEvent
		if err := rows.Scan(&event.EventID, &event.Subject, &event.Payload, &event.OccurredAt, &event.Attempts); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		found = append(found, event)
	}
	return found
}

// The property the outbox exists for: the announcement and the thing it
// announces commit together. A USR-* is minted once in the lifetime of an OIDC
// subject and cannot be re-derived from a later event, so an announcement lost
// to a broker that happened to be down is lost for good.
func TestAnIdentityAllocationAndItsAnnouncementCommitTogether(t *testing.T) {
	ctx, db := storeFixture(t)
	ctx = WithCorrelationID(ctx, "req-outbox-identity")

	subject := "outbox-identity-" + time.Now().UTC().Format("150405.000000000")
	globalID, created, err := db.EnsureIdentity(ctx, Identity{Subject: subject, Username: "outbox", Email: "outbox@example.invalid"})
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if !created {
		t.Fatalf("the fixture subject %q already existed", subject)
	}

	queued := queuedFor(t, ctx, db, globalID)
	if len(queued) != 1 {
		t.Fatalf("first sighting of %s queued %d events, want exactly 1", subject, len(queued))
	}
	event := queued[0]
	if event.Subject != "bsystem.events.identity.created" {
		t.Errorf("subject = %q; a consumer subscribed to bsystem.events.> would never see this", event.Subject)
	}
	envelope := decodeEnvelope(t, event.Payload)
	if envelope["event_id"] != event.EventID {
		t.Errorf("the envelope's event_id (%v) is not the row's (%s); a consumer deduplicating on it would not collapse a redelivery", envelope["event_id"], event.EventID)
	}
	if envelope["event"] != "identity.created" {
		t.Errorf("envelope event = %v, want identity.created", envelope["event"])
	}
	if envelope["request_id"] != "req-outbox-identity" {
		t.Errorf("request_id = %v; the event cannot be traced back to the request that caused it", envelope["request_id"])
	}
	if envelope["occurred_at"] == nil || envelope["occurred_at"] == "" {
		t.Error("the envelope has no occurred_at; a row delivered after an outage would look like it happened after the outage")
	}

	// Signing in again is not a second allocation and must not be a second
	// announcement.
	if _, createdAgain, err := db.EnsureIdentity(ctx, Identity{Subject: subject, Username: "outbox"}); err != nil || createdAgain {
		t.Fatalf("second EnsureIdentity: created = %v, err = %v", createdAgain, err)
	}
	if again := queuedFor(t, ctx, db, globalID); len(again) != 1 {
		t.Errorf("a repeat sign-in queued %d events for %s, want 1", len(again), globalID)
	}
}

// The handler used to publish global_id.created after every allocation call,
// including the two paths that return an identifier which already existed. So
// asking twice for the same source record announced two creations of a thing
// created once, and a consumer counting allocations counted requests.
func TestAGlobalIDThatAlreadyExistedIsNotAnnouncedAgain(t *testing.T) {
	ctx, db := storeFixture(t)
	sourceID := "outbox-repeat-" + time.Now().UTC().Format("150405.000000000")

	first, err := db.CreateGlobalEntity(ctx, "client", "espocrm", sourceID, "", nil)
	if err != nil {
		t.Fatalf("first allocation: %v", err)
	}
	again, err := db.CreateGlobalEntity(ctx, "client", "espocrm", sourceID, "", nil)
	if err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	if again.GlobalID != first.GlobalID {
		t.Fatalf("the same source record got two Global IDs: %s and %s", first.GlobalID, again.GlobalID)
	}
	queued := queuedFor(t, ctx, db, first.GlobalID)
	if len(queued) != 1 {
		t.Fatalf("two allocation calls for one source record queued %d events, want 1", len(queued))
	}
	if queued[0].Subject != "bsystem.events.global_id.created" {
		t.Errorf("subject = %q, want bsystem.events.global_id.created", queued[0].Subject)
	}
}

// The transaction is the whole mechanism, so the case where it rolls back is
// the one worth writing: an announcement must not survive a transaction that
// did not.
func TestAnEventQueuedInARolledBackTransactionDoesNotExist(t *testing.T) {
	ctx, db := storeFixture(t)

	eventID, err := NewEventID()
	if err != nil {
		t.Fatalf("new event id: %v", err)
	}
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := enqueueEvent(ctx, tx, OutboxEvent{
		EventID: eventID, Subject: "bsystem.events.identity.created",
		Payload: []byte(`{"event":"identity.created"}`), OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, present, _, err := db.OutboxEventByID(ctx, eventID); err != nil {
		t.Fatalf("read back: %v", err)
	} else if present {
		t.Error("an event queued by a transaction that rolled back is in the outbox; it announces something that never happened")
	}
}

// Claiming schedules the next attempt before the row is handed out, so a
// publisher that dies between claiming and delivering leaves the row due again
// rather than claimed forever. Nothing notices the crash, which is the point:
// there is no bookkeeping to get wrong.
func TestAClaimedRowThatIsNeverDeliveredBecomesDueAgain(t *testing.T) {
	ctx, db := storeFixture(t)
	eventID := enqueueForTest(t, ctx, db, "bsystem.events.identity.created")

	now := time.Now().UTC()
	claimed, err := db.ClaimDueEvents(ctx, 10, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !containsEvent(claimed, eventID) {
		t.Fatalf("a due event was not claimed")
	}

	// The publisher "crashes" here: no delivery, no failure recorded.
	immediately, err := db.ClaimDueEvents(ctx, 10, now)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if containsEvent(immediately, eventID) {
		t.Error("a just-claimed row was handed out again immediately; two publishers would deliver it in a tight loop")
	}

	later, err := db.ClaimDueEvents(ctx, 10, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("later claim: %v", err)
	}
	if !containsEvent(later, eventID) {
		t.Error("a claimed but undelivered row never became due again; the event is stranded")
	}
}

// Retrying is bounded. Unbounded retry is not durability, it is a queue that
// never drains and a log that never stops; a row that has exhausted its budget
// is marked failed and left where a metric can see it.
func TestTheAttemptBudgetIsBoundedAndTheRowIsKept(t *testing.T) {
	ctx, db := storeFixture(t)
	eventID := enqueueForTest(t, ctx, db, "bsystem.events.global_id.created")

	at := time.Now().UTC()
	for attempt := 0; attempt < MaxOutboxAttempts+2; attempt++ {
		if _, err := db.ClaimDueEvents(ctx, 10, at); err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		at = at.Add(30 * time.Minute)
	}

	var attempts int
	var failedAt *time.Time
	if err := db.pool.QueryRow(ctx, `SELECT attempts, failed_at FROM event_outbox WHERE event_id=$1`, eventID).Scan(&attempts, &failedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if failedAt == nil {
		t.Fatalf("after %d attempts the row is still being retried; the budget is not bounded", attempts)
	}
	if attempts > MaxOutboxAttempts+1 {
		t.Errorf("attempts = %d, more than the budget of %d", attempts, MaxOutboxAttempts)
	}
	if further, err := db.ClaimDueEvents(ctx, 10, at.Add(time.Hour)); err != nil {
		t.Fatalf("claim after failure: %v", err)
	} else if containsEvent(further, eventID) {
		t.Error("a failed row is still being claimed")
	}
}

// Delivery is terminal.
func TestADeliveredRowIsNotClaimedAgain(t *testing.T) {
	ctx, db := storeFixture(t)
	eventID := enqueueForTest(t, ctx, db, "bsystem.events.identity.created")

	if err := db.MarkDelivered(ctx, eventID, time.Now().UTC()); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	claimed, err := db.ClaimDueEvents(ctx, 10, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if containsEvent(claimed, eventID) {
		t.Error("a delivered event was claimed again; every consumer would receive it twice for as long as the row lives")
	}
}

// Two Cores run against one database in the E2E stack and are meant to in
// production. SKIP LOCKED is what makes that a faster drain rather than a
// doubled delivery.
func TestTwoPublishersDoNotClaimTheSameRow(t *testing.T) {
	ctx, db := storeFixture(t)
	ids := map[string]bool{}
	for i := 0; i < 5; i++ {
		ids[enqueueForTest(t, ctx, db, "bsystem.events.identity.created")] = true
	}

	now := time.Now().UTC()
	// Both claims happen against the same instant, which is the contended
	// case: the second must take what the first did not.
	first, err := db.ClaimDueEvents(ctx, 3, now)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := db.ClaimDueEvents(ctx, 3, now)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	seen := map[string]bool{}
	for _, event := range append(append([]OutboxEvent{}, first...), second...) {
		if !ids[event.EventID] {
			continue
		}
		if seen[event.EventID] {
			t.Errorf("%s was claimed by both publishers; it would be delivered twice", event.EventID)
		}
		seen[event.EventID] = true
	}
}

// An event id that collides is worse than no id at all: a consumer
// deduplicating on it would discard a different event.
func TestEventIDsAreUniqueAndWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id, err := NewEventID()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if seen[id] {
			t.Fatalf("%s was generated twice in %d draws", id, i)
		}
		seen[id] = true
		if len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Fatalf("%q is not a uuid; the column will refuse it", id)
		}
		if id[14] != '4' {
			t.Fatalf("%q is not a version 4 uuid", id)
		}
	}
}

func enqueueForTest(t *testing.T, ctx context.Context, db *DB, subject string) string {
	t.Helper()
	eventID, err := NewEventID()
	if err != nil {
		t.Fatalf("new event id: %v", err)
	}
	if err := db.EnqueueEvent(ctx, OutboxEvent{
		EventID: eventID, Subject: subject,
		Payload: []byte(`{"event":"test.queued"}`), OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return eventID
}

func containsEvent(events []OutboxEvent, eventID string) bool {
	for _, event := range events {
		if event.EventID == eventID {
			return true
		}
	}
	return false
}

// testAudit is the record a scope grant carries in tests.
//
// The grant and its record are one transaction now, so every test that grants
// a scope also writes an audit row. It is a helper rather than a literal at
// each site because the columns that matter are the ones the fail-closed
// policy relies on — who, and against what.
func testAudit(action string) AuditEvent {
	return AuditEvent{
		Subject:      "test-subject",
		GlobalUserID: "USR-000001",
		Action:       action,
		ResourceType: "client",
		ResourceID:   "CL-000001",
		RequestID:    "test-request",
	}
}
