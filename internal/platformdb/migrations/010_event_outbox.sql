-- The durable half of the platform's event delivery.
--
-- Events announcing an identity or Global ID allocation are written here in
-- the same transaction as the allocation itself, so a row exists if and only
-- if the thing it announces happened. Nothing is published from the request
-- path: a publisher loop drains this table, and a broker that is down delays
-- delivery instead of losing it.
--
-- Best-effort events are not written here. See docs/EVENTS.md for which
-- category each event is in and why.

CREATE TABLE IF NOT EXISTS event_outbox (
    -- Chosen by the platform and carried into the envelope, so a consumer can
    -- discard a duplicate by comparing ids. It is also the JetStream
    -- Nats-Msg-Id, which lets the broker collapse a redelivery of the same
    -- event within its duplicate window.
    event_id        uuid PRIMARY KEY,
    subject         text        NOT NULL,
    payload         jsonb       NOT NULL,
    -- When the thing happened, not when it was delivered. A row delivered
    -- after an outage must not look like it happened after the outage.
    occurred_at     timestamptz NOT NULL,
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    delivered_at    timestamptz,
    -- Set when the attempt budget is exhausted. A failed row is kept rather
    -- than deleted: the whole point of a durable event is that somebody can
    -- still find out it was never delivered.
    failed_at       timestamptz,
    last_error      text
);

-- The publisher's only query: the oldest rows that are due and not finished.
-- Partial, because delivered rows accumulate and are never scanned again.
CREATE INDEX IF NOT EXISTS event_outbox_due_idx
    ON event_outbox (next_attempt_at, occurred_at)
    WHERE delivered_at IS NULL AND failed_at IS NULL;
