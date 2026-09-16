# Support

`docs/openapi.yaml` is the authoritative contract. This document explains the
decisions behind it.

## One table for incidents and requests

An incident is something broken; a request is something wanted. They share a
lifecycle, a severity and an audience, so they share a table and differ by a
`kind` field. Splitting them would duplicate all three to express one word.

## The SLA targets are not in this repository

**This is the decision to read first.** The platform computes an SLA state; it
does not decide what the targets are. What the business promises a customer,
and what it owes when it misses, is a commercial commitment. Plausible-looking
defaults committed here would end up in front of customers as a promise nobody
made — and would be believed, because they would appear in the same field as a
real one.

`support_sla_policies` is therefore seeded empty. With no row for a severity,
the SLA state is `unset`, which is deliberately distinct from `on_track`: a
customer reading "on track" has been told a promise is being kept, and there is
no promise.

Filling that table is an owner decision, recorded as blocked in `TASKS.md`.
Everything else about the SLA works now and is tested: due dates derive from
creation plus the target, `at_risk` is the last fifth of whichever budget is
nearer, and a breach is a missed target rather than a judgement.

Two subtleties in the evaluation:

- **A resolved record's state stops moving.** It is judged on when it was
  resolved, not on the clock. Otherwise a report written a week later would
  show every closed incident as breached.
- **The response target stops binding once it is met.** A record acknowledged
  in time has kept that half of the promise whatever happens afterwards, so it
  is then judged only on resolution.

`acknowledged_at` and `resolved_at` are recorded the first time the status
reaches them and never moved again. Reopening a resolved record must not erase
that it was once resolved on time.

The state is computed on read rather than stored, because it is a function of
the clock and a stored value would be wrong from the moment it was written.

## Severity is not event severity

| Scale | Says |
| --- | --- |
| Event severity (`debug`…`critical`) | what happened |
| Incident severity (`low`…`critical`) | how quickly somebody will respond |

They are different questions, which is why the incident scale is smaller and
blunter, and why it is the one an SLA is keyed on. The mapping between them is
written out explicitly in `eventSeverityFor` rather than shared as a constant,
so changing one cannot silently change the other. An unrecognised severity maps
to the quietest event severity: an unknown value is not evidence of an
emergency.

**There is no default severity.** An unstated one is a question nobody has
answered, and answering it on the reporter's behalf picks their response time
for them.

## The lifecycle is a closed graph

```text
new ──► acknowledged ──► in_progress ──► resolved ──► closed
 │            │                │             │
 └────────────┴────────────────┴─────────────┴──────► closed
                                resolved ◄──┘ (reopen)
```

- **`closed` is terminal.** Reopening it would silently rewrite whatever has
  already been reported from it.
- **`resolved` may reopen to `in_progress`.** A fix that did not hold is the
  same incident, and opening a second record loses the history of the first.
- **Nothing returns to `new`.** It is the state of a record nobody has looked
  at, and that stops being true the moment somebody does.

A refused transition is a `409` naming both ends. A caller who sent a status
needs to know which *move* was refused, rather than being told the value was
wrong when it was not.

## Relations

A record may point at a client, server, project, task, bug or document. The
list is closed, and every relation is verified to resolve to an entity of the
named type before it is stored. A relation to something the platform cannot
identify is a dangling pointer that reads as a fact.

Relations are not loaded for a listing. A list is read to find a record, and
fetching every relation for every row would make the common case pay for the
uncommon one.

## Who can read what

`support.incident.read` to read, `support.incident.write` to raise or change.

A record naming a customer is authorized against that **client**, because that
is where a grant is written: "this customer may see their own incidents" is a
statement about the customer. Getting it backwards would mean a customer
granted access to their own client still could not see their own incidents, and
an administrator would be asked to write a grant per incident.

The Customer role holds `support.incident.read` and is scope-confined, so a
customer sees exactly the records a grant reaches and nothing else — and the
collection endpoint, which needs the permission platform-wide, refuses them
entirely. Listing everyone's incidents is a different question from reading
your own.

The detail endpoints refuse **before** they read, for anyone who cannot hold
the permission at all. A test asserts that ordering by reading the handler,
because both orderings return the same thing to an authorized caller; the
difference is only visible to a refused caller, and what they would see is
whether the record existed.

## Events

| Change | Event |
| --- | --- |
| created | `incident.created` |
| reached `resolved` | `incident.resolved` |
| anything else | `incident.updated` |

Only `incident.created` raises a notification. Resolution is good news and an
edit is routine; neither needs to interrupt anyone.

**The title travels in the event; the summary does not.** A title is a one-line
label somebody wrote to be read at a glance. A summary is where a reporter
pastes logs, addresses and occasionally a credential — and an event is
delivered far more widely than the record it came from.

Creation and update are both audited, so who changed an incident's severity or
closed it is answerable after the fact.
