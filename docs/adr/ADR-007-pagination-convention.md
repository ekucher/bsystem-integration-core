# ADR-007: Pagination convention

Status: Accepted

Date: 2026-09-16

## Context

The platform serves collections from four kinds of store, and they do not
paginate alike:

- **Upstream systems** (EspoCRM, Redmine, Outline) paginate by offset, because
  that is all their APIs offer.
- **Notifications** are append-heavy at the head.
- **Search** orders by relevance.
- **Servers and support records** are small, stable sets read by identifier.

A single convention imposed on all four would be wrong for at least two of
them. Four unrelated conventions would make every caller learn the platform
four times.

The failure that matters is not aesthetic. A caller walking a collection with
the wrong cursor kind sees one record twice and misses another, and does not
find out: the page looks complete either way.

## Decision

**One shape, several encodings.** Every collection returns the same envelope —
`data` plus `pagination` with `total`, `limit` and an optional `next_cursor` —
and a caller walks any collection the same way: follow `next_cursor` until it
is absent.

The cursor is **opaque**. Callers must not construct or parse one. That is what
lets each collection choose the encoding its store actually supports:

| Collection | Cursor | Why |
| --- | --- | --- |
| Upstream-backed | offset | it is what the upstream offers |
| Notifications | the last id (keyset) | the store grows at the head, and an offset would shift every later page on each arrival |
| Search | offset | relevance is a property of the query and the whole index, so there is no stable key to resume from |
| Servers, support | the last Global ID | already public, already immutable, and stable under insertion |

A cursor the platform did not issue is **rejected** with `invalid_cursor`
rather than reinterpreted. Guessing at it would silently return the wrong
window.

`limit` is **clamped, not refused**: a caller asking for more than the platform
serves gets the maximum. The cap is what stops one request pulling an unbounded
amount of upstream data.

`next_cursor` is issued only when more remains, so the walk always terminates.
A short page normally ends a collection — with one exception, below.

## Consequences

**Search carries no `total`, and may return a short page with a cursor.** Its
authorization filter removes candidates after the provider has counted them,
so the count would describe records the caller may not be allowed to know
exist, and a short page does not mean the end. This is the one place the
convention bends, and it bends for a disclosure reason rather than a technical
one.

Changing a collection's encoding later is not a breaking change, precisely
because the cursor is opaque. That is the property this ADR is buying: when an
upstream gains real cursors, only the adapter changes.

The cost is that a caller cannot jump to page 40, and cannot show "page 3 of
17" for upstream-backed collections without the `total` the upstream reports.
That is accepted: both are rare next to walking a list, and both push work onto
source systems that paginate by offset anyway.
