package platformdb

// The transactional outbox.
//
// Before this, every platform event was a fire-and-forget nc.Publish from the
// request path. That is a defensible contract for an event whose subject can
// be re-read from the API afterwards — a consumer that missed `incident.updated`
// can fetch the incident. It is not a defensible contract for an event that
// announces something which happens exactly once in the lifetime of a subject
// and cannot be re-derived: the first sighting of an OIDC subject, the first
// sighting of a service identity, the minting of a Global ID.
//
// Those three are written here, inside the same transaction as the allocation
// they announce. That is the property worth having: a row exists if and only
// if the allocation committed. An event cannot be published for a transaction
// that rolled back, and an allocation cannot commit while its announcement is
// lost to a broker that happened to be down.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/events"
	"github.com/jackc/pgx/v5"
)

// OutboxEvent is one durable event, queued or claimed.
type OutboxEvent struct {
	EventID    string
	Subject    string
	Payload    []byte
	OccurredAt time.Time
	Attempts   int
}

// OutboxCounts is the state of the queue, for metrics and for a readiness
// answer that can say "delivery is behind" rather than only "NATS is down".
type OutboxCounts struct {
	Queued  int
	Retried int
	Failed  int
}

// MaxOutboxAttempts bounds retrying.
//
// Unbounded retry is not durability, it is a queue that never drains and a log
// that never stops. A row that has failed this many times is marked failed and
// left in place, where a metric can see it and a person can decide.
const MaxOutboxAttempts = 12

// NewEventID returns an immutable identifier for one event.
//
// Random rather than derived from time: the platform's request ids are
// timestamps, and two events minted in the same nanosecond would share one.
// An id that collides is worse than no id at all, because a consumer
// deduplicating on it would discard a different event.
func NewEventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate event id: %w", err)
	}
	// RFC 4122 version 4, variant 10. The column is a uuid, so the shape is
	// not cosmetic.
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// enqueueEvent writes one durable event inside a caller's transaction.
//
// It takes the transaction rather than opening its own, which is the entire
// point: a separate connection would commit independently and reintroduce the
// window this file exists to close.
func enqueueEvent(ctx context.Context, tx pgx.Tx, event OutboxEvent) error {
	if event.EventID == "" || event.Subject == "" {
		return fmt.Errorf("outbox event needs an id and a subject")
	}
	occurred := event.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `
INSERT INTO event_outbox (event_id, subject, payload, occurred_at)
VALUES ($1,$2,$3::jsonb,$4)
ON CONFLICT (event_id) DO NOTHING`, event.EventID, event.Subject, string(event.Payload), occurred.UTC())
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", event.Subject, err)
	}
	return nil
}

// EnqueueEvent writes one durable event in its own transaction.
//
// For a caller that has no transaction of its own to join. It is deliberately
// not used by the identity and Global ID paths: joining their transaction is
// what makes the event and the allocation inseparable.
func (db *DB) EnqueueEvent(ctx context.Context, event OutboxEvent) error {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := enqueueEvent(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClaimDueEvents takes the next batch of undelivered events and schedules
// their next attempt before returning them.
//
// Scheduling the retry at claim time rather than on failure is what makes a
// crashed publisher safe. A process that dies between claiming a row and
// delivering it leaves the row due again after its backoff instead of leaving
// it claimed forever, and no bookkeeping is needed to notice the crash.
//
// SKIP LOCKED is what makes two Cores against one database safe: each takes
// rows the other is not holding, so the pair delivers faster rather than
// delivering everything twice.
func (db *DB) ClaimDueEvents(ctx context.Context, limit int, now time.Time) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
SELECT event_id, subject, payload, occurred_at, attempts
FROM event_outbox
WHERE delivered_at IS NULL AND failed_at IS NULL AND next_attempt_at <= $1
ORDER BY occurred_at
LIMIT $2
FOR UPDATE SKIP LOCKED`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	claimed := []OutboxEvent{}
	for rows.Next() {
		var event OutboxEvent
		if err := rows.Scan(&event.EventID, &event.Subject, &event.Payload, &event.OccurredAt, &event.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		claimed = append(claimed, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(claimed) == 0 {
		return nil, tx.Commit(ctx)
	}

	ids := make([]string, 0, len(claimed))
	for i := range claimed {
		claimed[i].Attempts++
		ids = append(ids, claimed[i].EventID)
	}
	// A row whose budget is now spent is failed here rather than on the next
	// pass, so the attempt that exhausts it is also the one that records it.
	if _, err := tx.Exec(ctx, `
UPDATE event_outbox
SET attempts = attempts + 1,
    -- Exponential, capped. The cap is what keeps a long outage from pushing
    -- the next attempt hours out and turning recovery into a wait.
    next_attempt_at = $2::timestamptz + (interval '1 second' * LEAST(power(2, LEAST(attempts, 8)), 256)),
    failed_at = CASE WHEN attempts + 1 >= $3 THEN $2::timestamptz ELSE NULL END
WHERE event_id = ANY($1)`, ids, now.UTC(), MaxOutboxAttempts); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

// MarkDelivered records a broker acknowledgement.
//
// Only an acknowledgement reaches here. A publish call that returned without
// one is a message the broker may never have stored, and treating it as
// delivered is exactly the silent loss the outbox exists to prevent.
func (db *DB) MarkDelivered(ctx context.Context, eventID string, at time.Time) error {
	_, err := db.pool.Exec(ctx, `
UPDATE event_outbox SET delivered_at=$2, last_error=NULL WHERE event_id=$1 AND delivered_at IS NULL`, eventID, at.UTC())
	return err
}

// MarkAttemptFailed records why an attempt failed and clears a premature
// failure verdict when the row still has budget left.
//
// The error text is the broker's, not a credential's: nothing in a NATS
// publish error carries platform authentication material, and the subject is
// already a column.
func (db *DB) MarkAttemptFailed(ctx context.Context, eventID string, reason string) error {
	const limit = 300
	if len(reason) > limit {
		reason = reason[:limit]
	}
	_, err := db.pool.Exec(ctx, `
UPDATE event_outbox SET last_error=$2 WHERE event_id=$1 AND delivered_at IS NULL`, eventID, reason)
	return err
}

// OutboxState reports the queue for metrics.
func (db *DB) OutboxState(ctx context.Context) (OutboxCounts, error) {
	var counts OutboxCounts
	err := db.pool.QueryRow(ctx, `
SELECT
 count(*) FILTER (WHERE delivered_at IS NULL AND failed_at IS NULL),
 count(*) FILTER (WHERE delivered_at IS NULL AND failed_at IS NULL AND attempts > 0),
 count(*) FILTER (WHERE failed_at IS NOT NULL)
FROM event_outbox`).Scan(&counts.Queued, &counts.Retried, &counts.Failed)
	return counts, err
}

// OutboxEventByID reads one row back. Tests use it; so does an operator
// answering "was this ever delivered".
func (db *DB) OutboxEventByID(ctx context.Context, eventID string) (OutboxEvent, bool, time.Time, error) {
	var event OutboxEvent
	var delivered *time.Time
	err := db.pool.QueryRow(ctx, `
SELECT event_id, subject, payload, occurred_at, attempts, delivered_at
FROM event_outbox WHERE event_id=$1`, eventID).
		Scan(&event.EventID, &event.Subject, &event.Payload, &event.OccurredAt, &event.Attempts, &delivered)
	if err != nil {
		if err == pgx.ErrNoRows {
			return OutboxEvent{}, false, time.Time{}, nil
		}
		return OutboxEvent{}, false, time.Time{}, err
	}
	if delivered == nil {
		return event, true, time.Time{}, nil
	}
	return event, true, *delivered, nil
}

// marshalEvent is the one place an envelope becomes bytes, so a durable event
// cannot be queued in a shape a consumer has never seen.
func marshalEvent(payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode event: %w", err)
	}
	return body, nil
}

// --- Correlation -------------------------------------------------------------

type correlationKey struct{}

// WithCorrelationID carries the request's X-Request-ID into the store layer.
//
// It travels in the context rather than as a parameter because the alternative
// is threading one string through sixteen call sites and every future one, and
// the call that forgets it is the call whose event cannot be traced back to
// the request that caused it. The middleware sets it once.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationKey{}, id)
}

func correlationFrom(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// queueDurableEvent builds an envelope and writes it inside the caller's
// transaction.
//
// The envelope is assembled here rather than by each caller so that every
// durable event carries the same fields: a consumer switching on `event` must
// not have to know which publisher built the message.
func queueDurableEvent(ctx context.Context, tx pgx.Tx, event, entityID, tenantID string, data map[string]any) error {
	eventID, err := NewEventID()
	if err != nil {
		return err
	}
	occurred := time.Now().UTC()
	envelope := events.Envelope{
		EventID:    eventID,
		Event:      event,
		Source:     EventSource,
		EntityID:   entityID,
		TenantID:   tenantID,
		Severity:   "info",
		OccurredAt: occurred,
		RequestID:  correlationFrom(ctx),
		Data:       data,
	}
	payload, err := marshalEvent(envelope)
	if err != nil {
		return err
	}
	return enqueueEvent(ctx, tx, OutboxEvent{
		EventID:    eventID,
		Subject:    events.Subject(event),
		Payload:    payload,
		OccurredAt: occurred,
	})
}

// EventSource identifies the platform as the publisher. Adapters name their
// upstream in `source`; events the platform itself originates name the
// platform.
const EventSource = "bsystem-integration-core"
