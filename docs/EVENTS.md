# Events

The platform publishes what happened to `bsystem.events.<event>` on NATS.

## Naming

`entity.action`, lower case, both segments required. A name outside that shape
is refused at publication rather than normalized into something plausible.

## Envelope

```json
{
  "event_id": "8f1d2b40-6c3a-4e57-9a11-2b7c5d0e4f38",
  "event": "backup.failed",
  "source": "bsystem-operations",
  "actor_id": "SVC-000001",
  "entity_id": "SRV-000004",
  "tenant_id": "CL-000001",
  "severity": "critical",
  "occurred_at": "2026-01-06T02:14:00Z",
  "request_id": "4f1c2a",
  "data": { "message": "nightly backup exited 1" }
}
```

`event_id` is minted by the platform, never by the publisher: a caller-supplied
id would let one service claim another's and make a consumer discard a real
event as a duplicate.

`actor_id` and `request_id` are filled from the calling identity and the
request correlation id when a publisher omits them, so an event can always be
traced back to the request that caused it.

`severity` is one of `debug`, `info`, `warning`, `error`, `critical`. Where the
platform has its own classification of an event, the publisher's severity is a
**floor**: it may escalate an event it knows is worse than usual, but cannot
quietly downgrade one the platform treats as critical. An unrecognised severity
is ignored rather than trusted.

## What travels in `data`

**As little as possible.** An event is delivered far more widely than the
record it came from — to every subscriber, into notifications, into whatever
consumes the bus later — and it is not authorized per subscriber.

The platform's own publishers put a single `message` in it, and consumers read
only that. An incident publishes its **title** and never its summary: a title
is a line somebody wrote to be read at a glance, while a summary is where a
reporter pastes logs, addresses and occasionally a credential.

## Published by the platform

| Event | Published when |
| --- | --- |
| `identity.created` | an authentik subject is seen for the first time (durable) |
| `service_identity.created` | a service identity is seen for the first time (durable) |
| `global_id.created` | a Global ID is minted for a source record (durable) |
| `incident.created` | a support record is raised |
| `incident.updated` | a support record changes |
| `incident.resolved` | a support record reaches `resolved` |
| `backup.succeeded`, `backup.failed` | reported by an operations reporter |
| `selftest.succeeded`, `selftest.failed` | reported by an operations reporter |
| `maintenance.started`, `maintenance.completed` | reported by an operations reporter |
| `server.ok`, `server.warning`, `server.error`, `server.offline` | reported by an operations reporter |

Any service identity holding `events.publish` may publish others through
`POST /api/service/v1/events`.

## Delivery guarantees

Every event is in one of two categories, and the category is decided by one
question: **can a consumer that missed this event recover what it says by
reading the platform afterwards?**

### Durable

| Event | Why |
| --- | --- |
| `identity.created` | a `USR-*` is minted once in the lifetime of an OIDC subject |
| `service_identity.created` | a `SVC-*` is minted once in the lifetime of a service subject |
| `global_id.created` | a Global ID is minted once per source record, and is immutable |

These three announce an allocation that happens exactly once and can never
happen again. There is no later event that restates it and no way for a
consumer to tell "I missed the announcement" from "it never happened", so
losing one is losing it for good.

They are written to a **transactional outbox** — a row in `event_outbox`,
inserted in the same transaction as the allocation itself. The row exists if
and only if the allocation committed: an event cannot be published for a
transaction that rolled back, and an allocation cannot commit while its
announcement is lost to a broker that happened to be down. Nothing is
published from the request path.

A publisher loop drains the table and publishes to **JetStream**, marking a row
delivered **only on a broker acknowledgement**. Core NATS has no per-message
ack — `nc.Publish` returns as soon as the bytes reach a socket buffer — so
treating that as delivery would put a durable table in front of a silent loss.

- Retries are exponential and **bounded** (`MaxOutboxAttempts`). A row that
  exhausts its budget is marked failed and **kept**, because the point of a
  durable event is that somebody can still find out it was never delivered.
- A publisher that dies between claiming a row and delivering it leaves the row
  due again after its backoff. There is no crash bookkeeping to get wrong.
- Two Cores against one database take disjoint rows (`FOR UPDATE SKIP LOCKED`),
  so a pair drains faster rather than delivering everything twice.
- A Core that starts while NATS is down still queues events and delivers them
  when the broker returns.

### Best effort

Everything else: the support events, the operations reporter's events, and
anything a service identity publishes through `POST /api/service/v1/events`.

Each of these describes a record that stays readable through the API, so a
consumer that missed one can fetch the current state. Publication never fails
the request that caused it: an event is a description of something that already
happened, and refusing the change because the description could not be
delivered would lose the change to make a secondary effect look atomic.

That is a real trade, not a shrug. **Nothing may treat the bus as the only
record of a change.** The database is the record; the bus is the notification.

### The loss and duplication windows

| | Loss | Duplication | Ordering |
| --- | --- | --- | --- |
| Durable | none while PostgreSQL holds the row; a row that exhausts its retry budget is failed and visible, never dropped | possible: a redelivery after an acknowledgement lost on the way back. JetStream collapses it inside a 5-minute duplicate window by `Nats-Msg-Id`, and `event_id` is the consumer's own defence outside it | by `occurred_at` within one publisher; none across publishers |
| Best effort | the whole broker outage: events produced while NATS is unreachable are dropped | none from the platform; NATS itself may redeliver | none |

### Idempotency expected of consumers

Every published envelope carries `event_id`, immutable and unique to one
occurrence. **A consumer that sees the same `event_id` twice has seen the same
event twice** and must treat the second as a no-op. That is the whole contract:
the platform does not promise exactly-once delivery, it promises an identifier
that makes exactly-once *processing* possible.

`occurred_at` is when the thing happened, not when it was delivered. A durable
event delivered after a two-hour outage carries the time of the allocation, so
a consumer ordering by it does not place the event after the outage.

### Metrics

- `bsystem_events_published_total{event,outcome}` — best-effort publication.
- `bsystem_event_outbox_attempts_total{subject,outcome}` — durable delivery
  attempts. `ack_not_recorded` is the one that needs a person: the broker has
  the event and the platform could not write that down, so it will be published
  again and the consumer's deduplication is what keeps it correct.
- `bsystem_event_outbox_events{state}` — queued, retrying, failed. A queued
  count that keeps climbing is a broker outage being survived. A failed count
  above zero is an event nobody will ever receive.

## Notifications

Eight events raise a notification for the people who act on them:
`backup.failed`, `server.offline`, `server.error`, `selftest.failed`,
`build.failed`, `test.failed`, `incident.created` and `release.created`.

Everything else raises none. That default is deliberate: every entry in that
table is a decision that some group of people wants to be interrupted, and a
platform that notifies on everything trains people to ignore it. See
`NOTIFICATIONS.md`.

## Consuming

Subjects are `bsystem.events.<event>`, so `bsystem.events.>` subscribes to
everything and `bsystem.events.backup.*` to one family.

A consumer must tolerate duplicates, and gaps in the best-effort events. There
is no ordering guarantee across events. `occurred_at` is the publisher's clock
for a published envelope and the platform's own for the three durable events.

Deduplicate on `event_id`. See **Delivery guarantees** above for which events
survive a broker outage and which do not.
