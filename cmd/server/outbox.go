package main

// The outbox publisher.
//
// Durable events are written to PostgreSQL inside the transaction that caused
// them (see internal/platformdb/outbox.go). Nothing is published from the
// request path: this loop drains the table, and a broker that is down delays
// delivery rather than losing it.
//
// Delivery is marked only on a JetStream acknowledgement. Core NATS has no
// per-message ack — nc.Publish returns as soon as the bytes are written to a
// socket buffer — so treating it as delivery would reintroduce exactly the
// silent loss this exists to prevent, with a durable table in front of it for
// reassurance.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// EventStreamName is the JetStream stream that stores platform events.
const EventStreamName = "BSYSTEM_EVENTS"

// eventPublisher delivers one event and reports whether the broker
// acknowledged it. It is an interface so the retry, backoff and bookkeeping
// can be tested against a broker that fails on demand, which a real one will
// not do reliably.
type eventPublisher interface {
	PublishEvent(ctx context.Context, subject, eventID string, payload []byte) error
}

// jetStreamPublisher publishes with an acknowledgement and a message id.
//
// The message id is the platform's own event id, which lets the broker
// collapse a redelivery of the same event inside its duplicate window. That
// covers the one case retrying cannot: an acknowledgement lost on the way
// back, where the event is stored and the outbox believes it is not.
type jetStreamPublisher struct {
	conn *nats.Conn

	mu     sync.Mutex
	stream jetstream.JetStream
	ready  bool
}

func newJetStreamPublisher(conn *nats.Conn) *jetStreamPublisher {
	return &jetStreamPublisher{conn: conn}
}

// ensure creates the stream if it is not there yet.
//
// It is attempted per publish rather than once at startup because a Core that
// started while NATS was down would otherwise never get a stream, and the
// outbox would fill behind a broker that came back minutes later.
func (p *jetStreamPublisher) ensure(ctx context.Context) (jetstream.JetStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready {
		return p.stream, nil
	}
	if p.conn == nil || !p.conn.IsConnected() {
		return nil, errors.New("not connected to NATS")
	}
	stream, err := jetstream.New(p.conn)
	if err != nil {
		return nil, fmt.Errorf("open JetStream: %w", err)
	}
	// Idempotent: a stream that already matches is returned unchanged, so two
	// Cores against one broker do not fight over it.
	if _, err := stream.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     EventStreamName,
		Subjects: []string{"bsystem.events.>"},
		Storage:  jetstream.FileStorage,
		// Events are a record of what happened, not a work queue: a consumer
		// reading them does not remove them for anybody else.
		Retention: jetstream.LimitsPolicy,
		MaxAge:    30 * 24 * time.Hour,
		// Long enough to cover a redelivery caused by a lost acknowledgement,
		// short enough that the broker is not asked to remember every event
		// id forever.
		Duplicates: 5 * time.Minute,
	}); err != nil {
		return nil, fmt.Errorf("ensure stream %s: %w", EventStreamName, err)
	}
	p.stream, p.ready = stream, true
	return stream, nil
}

func (p *jetStreamPublisher) PublishEvent(ctx context.Context, subject, eventID string, payload []byte) error {
	stream, err := p.ensure(ctx)
	if err != nil {
		return err
	}
	if _, err := stream.Publish(ctx, subject, payload, jetstream.WithMsgID(eventID)); err != nil {
		// A broker that went away takes its stream handle with it: the next
		// attempt re-creates both rather than publishing into a handle whose
		// server has been restarted with no stream.
		p.mu.Lock()
		p.ready = false
		p.mu.Unlock()
		return err
	}
	return nil
}

// outboxBatch is how many events one pass may deliver. Bounded so a backlog
// drains steadily instead of one pass holding a pool connection for minutes.
const outboxBatch = 50

// deliverOutbox makes one pass over the due events.
//
// It returns the number delivered, which is what the loop uses to decide
// whether to come back immediately: a backlog should drain at the speed of the
// broker, not at the speed of the tick.
func (a *app) deliverOutbox(ctx context.Context, publisher eventPublisher, now time.Time) (int, error) {
	claimed, err := a.db.ClaimDueEvents(ctx, outboxBatch, now)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, event := range claimed {
		if err := publisher.PublishEvent(ctx, event.Subject, event.EventID, event.Payload); err != nil {
			outboxAttempts.Inc(event.Subject, "failed")
			// Recorded, not logged per event: a broker that is down produces
			// one of these per queued event per attempt, and a log that
			// large is a log nobody reads.
			if markErr := a.db.MarkAttemptFailed(ctx, event.EventID, err.Error()); markErr != nil {
				return delivered, markErr
			}
			continue
		}
		if err := a.db.MarkDelivered(ctx, event.EventID, time.Now().UTC()); err != nil {
			// The broker has it and the platform failed to record that. The
			// row stays due, the next attempt republishes with the same
			// message id, and the broker's duplicate window collapses it.
			outboxAttempts.Inc(event.Subject, "ack_not_recorded")
			return delivered, err
		}
		outboxAttempts.Inc(event.Subject, "delivered")
		delivered++
	}
	return delivered, nil
}

// runOutbox drains the outbox until the context is cancelled.
func (a *app) runOutbox(ctx context.Context, publisher eventPublisher, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		delivered, err := a.deliverOutbox(ctx, publisher, time.Now().UTC())
		if err != nil && ctx.Err() == nil {
			logger.Error("outbox delivery pass failed", "error", err.Error())
		}
		// A full batch means there is more waiting. Coming back on the tick
		// would drain a thousand-row backlog at fifty rows per interval.
		if delivered >= outboxBatch {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// outboxInterval is how often the publisher looks for work when it has none.
func outboxInterval() time.Duration {
	return durationEnv("EVENT_OUTBOX_INTERVAL", 2*time.Second)
}
