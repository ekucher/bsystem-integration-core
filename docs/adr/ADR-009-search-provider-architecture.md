# ADR-009: Search provider architecture

Status: Accepted

Date: 2026-09-16

## Context

Search is the endpoint most likely to disclose something by accident. Every
other read starts from an identifier the caller already had; a search returns
records they have never named, drawn from every module at once.

BSYSTEM has no search engine deployed and no decision about which one it will
run. The obvious paths were both bad:

- **Wait for the engine.** Search stays unbuilt, and the authorization
  behaviour — the hard part — gets designed under deadline once a cluster
  exists.
- **Build against the engine we expect.** Index mappings, lifecycle and
  reindexing get guessed at, and the guesses become contract tests that pass
  because they test the guess.

## Decision

**A provider interface with two implementations, and the authorization
decision outside both.**

`Provider` has four methods: `Name`, `Index`, `Delete`, `Search`. `Index` is an
upsert keyed by Global ID and `Delete` is idempotent, so an indexer that
replays its work converges rather than duplicating or failing.

### The in-memory provider is the default

Not a placeholder. It scans, ranks deterministically, and exists so that
matching, filtering, pagination and above all the authorization boundary can be
tested and exercised end to end without an engine to deploy.

A deployment with no `OPENSEARCH_URL` gets it, so search is always answerable:
an empty index truthfully returns nothing. What never blurs is the difference
between an empty index and an engine that could not be reached — a provider
failure is a 503, because a user told their search found nothing will act on
it.

### The OpenSearch provider is a skeleton, precisely scoped

Present and contract-tested against a fake cluster: request and response
shapes, bulk NDJSON framing, error normalization, the resilience policy, and
the authorization narrowing pushed into the query.

Deliberately absent: index mappings, index lifecycle, reindexing. Those depend
on cluster decisions nobody has made, and inventing them would produce tests
that pass against the invention.

It builds real requests rather than returning stubs, so the first person to
point it at a cluster is debugging a cluster rather than this code.

### Two filters, and only the second is a decision

The provider narrows by permission and type **inside the query**, so a caller's
page is not assembled by fetching records they cannot see and discarding them.

The platform then evaluates **every returned candidate** through the same
authorization rules the rest of the API uses, with the document's own scope.

This is not redundancy. The provider's narrowing is a set intersection over an
index field; authorization also depends on scope grants and on whether the
principal's role is confined. A provider cannot make that decision, and a
search endpoint is the last place to let one try. The case it exists for: a
Customer's `crm.client.read` means "my own record", not "clients" — without
per-document evaluation, search would hand every customer every client in the
platform.

### The document carries its own audience

`permissions` and `scope_type`/`scope_id` travel with the indexed document
rather than being reconstructed at query time from whatever the result happens
to contain. A document with no permissions is refused at index time, not stored
and hidden: it would be indexed and never returned, and the publisher should
learn that now.

## Consequences

Switching engines changes relevance quality, not behaviour: both providers
weight title above summary, and neither decides who sees what.

Every result costs an authorization evaluation, which for a scope-confined
principal is a database lookup. This is bounded by over-fetching a fixed
multiple of the page and stopping once the page is full, and it is a cost the
design accepts: the alternative is trusting an index to enforce a tenant
boundary.

The index holds no upstream payload — title and summary are the only text —
so a search index cannot become a second copy of the source systems with
weaker access rules.
