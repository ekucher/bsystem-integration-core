# Events

The platform publishes what happened to `bsystem.events.<event>` on NATS.

## Naming

`entity.action`, lower case, both segments required. A name outside that shape
is refused at publication rather than normalized into something plausible.

## Envelope

```json
{
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
| `identity.created` | an authentik subject is seen for the first time |
| `service_identity.created` | a service identity is seen for the first time |
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

**Publication is best effort and never fails the request that caused it.** An
event is a description of something that already happened; refusing the change
because the description could not be delivered would lose the change to make a
secondary effect look atomic.

That is a real trade, not a shrug: a subscriber can miss an event, so nothing
may treat the bus as the only record of a change. The database is the record;
the bus is the notification. Outcomes are counted on
`bsystem_events_published_total` by event and result, because "nothing happened"
and "everything failed to publish" look identical on a dashboard that counts
only successes.

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

A consumer must tolerate duplicates and gaps. There is no ordering guarantee
across events, and `occurred_at` is the publisher's clock rather than the
platform's.
