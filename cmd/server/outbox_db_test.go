package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// recordingPublisher is a broker that fails on demand.
//
// The delivery loop's whole job is what it does when the broker does not
// answer, and a real broker will not refuse on cue. It also records what it
// was asked to publish, which is how the message id is checked: JetStream
// deduplication is only as good as the id the platform hands it.
type recordingPublisher struct {
	mu        sync.Mutex
	failures  int
	published []struct {
		Subject, EventID string
		Payload          []byte
	}
}

func (p *recordingPublisher) PublishEvent(_ context.Context, subject, eventID string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures > 0 {
		p.failures--
		return errors.New("broker unavailable")
	}
	p.published = append(p.published, struct {
		Subject, EventID string
		Payload          []byte
	}{subject, eventID, payload})
	return nil
}

func (p *recordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

// The claim about durability, executed: an event produced while the broker is
// unreachable is delivered once the broker returns. Before the outbox this was
// a metric labelled "unavailable" and a dropped message.
func TestAnEventProducedDuringABrokerOutageIsDeliveredAfterIt(t *testing.T) {
	application, handler := integrationApp(t, map[string]map[string]any{
		"admin": principal("outbox-admin", "BSYSTEM Administrators"),
	})
	ctx := context.Background()

	// Signing in for the first time allocates a USR-* and queues its
	// announcement inside that transaction.
	if recorder := call(t, handler, http.MethodGet, "/api/v1/me", "admin"); recorder.Code != http.StatusOK {
		t.Fatalf("sign in: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// The broker is down for the first two passes.
	broker := &recordingPublisher{failures: 2}
	for attempt := 0; attempt < 2; attempt++ {
		delivered, err := application.deliverOutbox(ctx, broker, time.Now().UTC().Add(time.Duration(attempt)*time.Hour))
		if err != nil {
			t.Fatalf("pass %d: %v", attempt, err)
		}
		if delivered != 0 {
			t.Fatalf("pass %d reported %d delivered while the broker was refusing", attempt, delivered)
		}
	}

	state, err := application.db.OutboxState(ctx)
	if err != nil {
		t.Fatalf("outbox state: %v", err)
	}
	if state.Queued == 0 {
		t.Fatal("nothing is queued after a broker outage; the event was lost rather than delayed")
	}

	// The broker returns.
	delivered, err := application.deliverOutbox(ctx, broker, time.Now().UTC().Add(4*time.Hour))
	if err != nil {
		t.Fatalf("delivery after recovery: %v", err)
	}
	if delivered == 0 {
		t.Fatal("nothing was delivered after the broker returned")
	}
	if broker.count() == 0 {
		t.Fatal("the broker received nothing")
	}

	found := false
	for _, message := range broker.published {
		if message.Subject != "bsystem.events.identity.created" {
			continue
		}
		found = true
		var envelope map[string]any
		if err := json.Unmarshal(message.Payload, &envelope); err != nil {
			t.Fatalf("decode delivered envelope: %v", err)
		}
		// The message id handed to the broker has to be the event's own id.
		// A per-attempt id would make the broker's duplicate window useless
		// against exactly the case it exists for: a redelivery after an
		// acknowledgement was lost on the way back.
		if envelope["event_id"] != message.EventID {
			t.Errorf("message id %q is not the envelope's event_id %v", message.EventID, envelope["event_id"])
		}
		if envelope["source"] != platformdb.EventSource {
			t.Errorf("source = %v, want %s", envelope["source"], platformdb.EventSource)
		}
	}
	if !found {
		t.Errorf("identity.created was never delivered; the broker saw %d messages", broker.count())
	}

	// And nothing is left behind.
	after, err := application.db.OutboxState(ctx)
	if err != nil {
		t.Fatalf("outbox state: %v", err)
	}
	if after.Queued != 0 {
		t.Errorf("%d events are still queued after a successful pass", after.Queued)
	}
}

// Delivery is marked on an acknowledgement and on nothing else. A publish call
// that returned an error must leave the row exactly as deliverable as it was,
// because the alternative is a durable table in front of a silent loss.
func TestAFailedPublishLeavesTheEventDeliverable(t *testing.T) {
	application, handler := integrationApp(t, map[string]map[string]any{
		"admin": principal("outbox-failure", "BSYSTEM Administrators"),
	})
	ctx := context.Background()
	if recorder := call(t, handler, http.MethodGet, "/api/v1/me", "admin"); recorder.Code != http.StatusOK {
		t.Fatalf("sign in: status = %d", recorder.Code)
	}

	broker := &recordingPublisher{failures: 1}
	if _, err := application.deliverOutbox(ctx, broker, time.Now().UTC()); err != nil {
		t.Fatalf("failing pass: %v", err)
	}
	state, err := application.db.OutboxState(ctx)
	if err != nil {
		t.Fatalf("outbox state: %v", err)
	}
	if state.Queued != 1 || state.Retried != 1 {
		t.Fatalf("after one refused attempt the outbox holds %+v, want one queued and retrying", state)
	}
	if state.Failed != 0 {
		t.Errorf("one refusal marked the event permanently failed: %+v", state)
	}
}

// A backlog drains at the speed of the broker rather than at the speed of the
// tick. The loop comes straight back when a pass filled its batch.
func TestAFullBatchIsFollowedImmediatelyByAnotherPass(t *testing.T) {
	application, _ := integrationApp(t, map[string]map[string]any{})
	ctx := context.Background()

	for i := 0; i < outboxBatch+5; i++ {
		id, err := platformdb.NewEventID()
		if err != nil {
			t.Fatalf("event id: %v", err)
		}
		if err := application.db.EnqueueEvent(ctx, platformdb.OutboxEvent{
			EventID: id, Subject: "bsystem.events.global_id.created",
			Payload: []byte(`{"event":"global_id.created"}`), OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	broker := &recordingPublisher{}
	// An interval long enough that a loop waiting for the tick would not
	// deliver the remainder within the deadline below.
	loopCtx, stop := context.WithCancel(ctx)
	go application.runOutbox(loopCtx, broker, time.Minute)
	deadline := time.Now().Add(20 * time.Second)
	for broker.count() < outboxBatch+5 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	stop()

	if got := broker.count(); got < outboxBatch+5 {
		t.Errorf("delivered %d of %d within the deadline; a full batch did not bring the loop straight back", got, outboxBatch+5)
	}
}
