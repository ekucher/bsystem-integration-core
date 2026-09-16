# Notifications

A notification is the platform telling someone that something happened. It is
not a message one user sends another: no human role can raise one.

`docs/openapi.yaml` is the authoritative contract. This document explains the
two decisions behind it that the schema cannot.

## Who a notification is addressed to

BSYSTEM has no authoritative mapping from an operational event to a person.
Nobody has told the platform who owns a build, who is on call for a backup, or
which humans want to hear about an incident. Inventing one would produce
notifications addressed to people who never asked for them, and would create a
second authorization system quietly disagreeing with the first.

So a notification is addressed one of two ways:

| Addressing | Set when | Resolved |
| --- | --- | --- |
| `recipient_id` | the publisher knows the person | never — it names one Global user ID |
| `audience_permission` | the publisher knows only the concern | from RBAC, each time it is read |

Exactly one is set; the database enforces it with a check constraint.

The audience form is resolved at read time rather than raise time, which has a
consequence worth stating: revoking someone's permission also withdraws the
notifications that permission entitled them to see. A fan-out computed when the
event arrived would have left them readable forever.

## Who may read one

`GET /api/v1/notifications` declares no route permission. The audience filter is
applied inside the query, against the same principal the rest of the platform
authorizes with, so a route permission could only be broader than that filter.

The filter is:

- a notification naming the caller's Global user ID is always visible;
- an administrator (`*`) reads every audience;
- any other principal reads the audiences its permissions name;
- **a scope-confined principal reads only what names it.**

The last rule is the one that matters. The Customer role is scope-confined
because customer-to-tenant ownership is not yet authoritative (see
`adr/ADR-005`). Its role permissions describe what it may do *inside scopes it
has been granted* — `support.incident.read` means "incidents I own", not
"incidents". Treating that as an audience would hand a customer every
platform-wide incident notification, with the tenant boundary leaking through a
side channel that no endpoint test would catch. An undecided audience denies.

Read state is per user, stored separately, so one person marking an audience
notification read does not hide it from everyone else.

A notification the caller may not see is answered exactly as one that does not
exist, both on read and on mark-read. Distinguishing them would let a caller
enumerate notification identifiers belonging to other people.

## Which events raise one

An event that is not in this table raises no notification at all. Every entry
is a decision that some group of people wants to be interrupted, and the
default for an unknown event is to interrupt nobody — a platform that notified
on everything would train people to ignore it.

| Event | Audience | Severity floor |
| --- | --- | --- |
| `backup.failed` | `operations.server.read` | critical |
| `server.offline` | `operations.server.read` | critical |
| `build.failed` | `development.repo.read` | error |
| `test.failed` | `qa.testcase.read` | error |
| `selftest.failed` | `operations.server.read` | error |
| `server.error` | `operations.server.read` | error |
| `incident.created` | `support.incident.read` | error |
| `release.created` | `development.repo.read` | info |

An audience must be a permission some unconfined role actually holds, not
merely one RBAC defines. `test.failed` was first addressed to `qa.report.read`,
which exists but which only the Manager role holds — the notification would
never have reached the QA team, and nothing would have failed. It would simply
have been invisible to the people it was for. A unit test now asserts that
every audience has a holder, and names the events each role is interrupted by.

Severity is a floor, not a value: a publisher may escalate an event it knows is
worse than usual, but cannot quietly downgrade one the platform considers
critical. An unrecognised severity from a publisher is ignored rather than
trusted.

Only the event's `data.message` becomes the notification body. Copying a whole
upstream payload would place potentially confidential data in front of everyone
holding the audience permission.

Raising the notification happens after the event is on the bus and does not
fail the publish. The event has already been accepted and delivered by then;
refusing it to make a secondary effect look atomic would lose the event too.
The failure is logged and counted on
`bsystem_notifications_raised_total{outcome="failed"}` instead — a mapped event
that silently fails to become a notification looks exactly like a quiet week.

## Deep links

`deep_link` is a HUB path, and is present only for entity types the HUB has a
page for: clients, contacts, projects, issues and documents. Operational and
support entities (`SRV-`, `INC-`, `BUG-`, `TST-`, `REL-`) have no link until
those areas of the HUB exist. A user follows a link, so one that leads nowhere
is worse than none.

## Pagination

Notifications page by the id of the last row returned, not by an offset. The
store is append-heavy at the head, and an offset cursor would make every new
arrival shift the later pages: a caller walking the collection would see one
notification twice and miss another. Keyset paging names a position rather than
a distance, so it is stable under insertion.

`total` and `unread_count` describe everything visible to the caller rather
than the current page, because an unread badge otherwise has to walk the whole
history to draw a number.
