# Operations

`docs/openapi.yaml` is the authoritative contract. This document explains the
decisions behind it.

(For the platform's own health, readiness and metrics, see `OBSERVABILITY.md`.
This is about the infrastructure the platform records, not about the platform.)

## The platform does not monitor anything

Reports arrive from an operations toolkit — BRAVO in this deployment — and the
platform normalizes, stores, publishes and notifies. The direction is
deliberate. A platform that polled infrastructure would be a second monitoring
system with its own idea of what "up" means, disagreeing with the one the
on-call engineer is actually looking at, and the disagreement would surface
during an incident.

So the contract is push-shaped: `POST /api/service/v1/servers` registers a
host, `POST /api/service/v1/operations/events` says what happened to it. Both
require `operations.report`, which no human role holds — a person does not
assert that a backup failed.

## A server carries no way to reach it

There is no hostname, no address, no port. The platform never connects to a
server, so it does not need one, and an inventory of reachable addresses is
exactly the kind of internal topology that should not accumulate somewhere more
people can read than can log in.

`name` is a label for humans. Identity is the Global ID, allocated by the
platform and keyed by the reporter's own identifier, so re-registering the same
host returns the same `SRV-*` rather than minting another.

`client_id` and `project_id` are verified to resolve before anything is stored.
An operations record attached to the wrong customer is worse than one attached
to none, and an unresolvable Global ID is the ambiguous ownership the platform
is required to refuse rather than guess at.

**Status is not settable at registration.** It is a consequence of reported
events; letting a registration set it would let a reporter declare a server
healthy without saying anything happened.

## The vocabulary is closed

| Event | Severity floor | Moves status to |
| --- | --- | --- |
| `backup.succeeded` | info | — |
| `backup.failed` | critical | — |
| `selftest.succeeded` | info | — |
| `selftest.failed` | error | — |
| `maintenance.started` | info | `maintenance` |
| `maintenance.completed` | info | `ok` |
| `server.ok` | info | `ok` |
| `server.warning` | warning | `warning` |
| `server.error` | error | `error` |
| `server.offline` | critical | `offline` |

An event outside the table is refused rather than stored. An operations feed
that accepts any name becomes a log, and nobody can write a query or a
notification rule against a log whose contents are unbounded.

### Why a failed backup does not mark the server broken

This is the distinction the module turns on. A backup outcome and a self-test
outcome say something about a *service the server runs*; they say nothing about
whether the server is up. Recording `backup.failed` as "this server is in
error" would put a healthy machine on a dashboard as broken and send somebody
to look at the wrong thing — while the actual problem, a backup that is not
running, gets read as a symptom of the machine rather than as itself.

`maintenance.completed` returns the server to `ok` rather than to whatever it
was before, because the operator has just finished working on it and is
asserting it is fine. Restoring a pre-maintenance `error` would contradict them.

`last_event_at` moves for *every* report, including the ones that leave status
alone, because "nothing has been heard from this server" is itself an
operational fact and a stale timestamp is how an operator learns a reporter has
gone quiet.

Severity is a floor: a reporter may escalate an event it knows is worse than
usual, but cannot lower one the platform treats as critical. An unrecognised
severity is ignored rather than trusted.

Only `summary` is stored as free text. A reporter's full payload can carry
credentials, addresses and command output, and this record is read by everyone
holding `operations.server.read`.

## Who can read what

`GET /api/v1/servers` and `GET /api/v1/operations/events` require
`operations.server.read` across the platform. A principal granted that
permission only inside one scope is refused there and can still read its own
servers individually: listing everyone's is a different question from reading
your own.

`GET /api/v1/servers/{id}` evaluates against the server's **client** when it
has one, because that is where a grant would naturally be written — "this
customer may see their own infrastructure" is a statement about the customer,
not about each machine. A server with no owner is evaluated against itself, so
a grant can still name exactly one host. Getting this backwards would mean an
administrator had to write a grant per machine, which is how a scope model
stops being used.

The refusal happens **before** the lookup. A caller who cannot hold
`operations.server.read` anywhere is refused without the platform reading
anything, so they get the same answer for a server that exists and one that
does not. Looking up first would let them enumerate which Global IDs name
infrastructure — a database they were refused access to, read one bit at a
time. A scope-confined caller is exempt from that early refusal, because its
authority is per-resource and has to be evaluated against the resolved server;
when that evaluation denies, it is told the server does not exist, since
"exists but not yours" is what it would use to enumerate its neighbours'
infrastructure.

A test asserts the ordering by reading the handler, because the bug it guards
against is invisible in the responses of a correctly authorized caller.

## What a report causes

A stored report becomes a platform event on `bsystem.events.<event>`, and for
the four events people act on — `backup.failed`, `selftest.failed`,
`server.error`, `server.offline` — a notification addressed to
`operations.server.read`. Successes and routine maintenance are stored and
published but interrupt nobody: a platform that notifies on everything trains
people to ignore it.

Both are best effort and neither fails the request. The report is already
stored by then, and losing it to make a secondary effect look atomic would be
the worse trade. Reports are counted on `bsystem_operations_events_total`,
because a reporter that has gone quiet looks identical to infrastructure with
nothing to report.

A test asserts the operations vocabulary and the notification table agree,
since they are maintained separately and nothing else would notice them
drifting apart.

## The BRAVO contract

What BRAVO must do to integrate:

1. `POST /api/service/v1/servers` for each host it knows about, with a stable
   `source_id`. Repeat whenever the name, environment or relations change; it
   is an upsert.
2. `POST /api/service/v1/operations/events` for each thing that happens, using
   a name from the vocabulary above.
3. Authenticate as a service identity in `BSYSTEM-Services` holding
   `operations.report`.

Nothing in this contract depends on BRAVO's own API, which is why it could be
built without it. **The other direction — the platform pulling an inventory or
a history out of BRAVO — is not specified here and should not be guessed at.**
It needs BRAVO's API documented first, and an adapter written against it would
otherwise be a fiction that compiles.
