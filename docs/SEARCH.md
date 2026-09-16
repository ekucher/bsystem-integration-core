# Search

`docs/openapi.yaml` is the authoritative contract. This document explains the
decisions behind it.

## Why search is different

Every other read in the platform starts from an identifier the caller already
had. A search returns records the caller has never named, drawn from every
module at once. That makes it the endpoint most likely to disclose something by
accident, so the shape of a searchable document is built around the question
"who may see this", and the answer travels with the document rather than being
reconstructed at query time from whatever the result happens to contain.

## What is indexed

A document carries **no upstream payload**. `title` and `summary` are the only
text, and both are expected to be safe to show to anyone who passes the
document's own authorization. A search index is read by more people than any
single endpoint; copying a source record into it would put upstream fields —
some of them confidential — in front of all of them.

Each document declares:

| Field | Meaning |
| --- | --- |
| `id` | the Global ID, which is also the index key |
| `permissions` | the permissions that grant read access; any one is enough |
| `scope_type`, `scope_id` | the scope a grant would be written against |

A document with no permissions is **refused at index time**, not stored and
hidden. It would be indexed and never returned, and the publisher should learn
that now rather than when somebody reports results going missing.

## How a result is authorized

Two filters run, and only the second is a decision.

1. The **provider narrows** by permission and type inside the query, so a
   caller's page is not assembled by fetching records they cannot see and
   discarding them.
2. The **platform evaluates** every returned candidate through the same
   authorization rules the rest of the API uses, with the document's own scope.

The second is not redundancy for its own sake. The provider's narrowing is a
set intersection over a field in an index; authorization also depends on scope
grants and on whether the principal's role is confined. A provider cannot make
that decision, and a search endpoint is the last place to let one try.

This is what the design exists for: a Customer's `crm.client.read` means "my own
record", not "clients". Without per-document evaluation, a search would hand
every customer every client in the platform. A scope-confined principal sees a
result only where an explicit grant reaches it.

A scope lookup that fails is a request failure, never a quiet denial: returning
a shorter list would present an incomplete result as a complete one.

## There is no total

The provider knows how many documents matched before authorization was applied.
That number counts records the caller may not be allowed to know exist — a
customer searching for a competitor's name and being told "47 results" has
learned something, whatever the page then shows. So the search envelope carries
no total, only a limit and a cursor, which is all a total would have been used
for.

For the same reason a page shorter than the requested limit can still carry a
`next_cursor`: candidates are removed after the provider has counted them.

## Pagination

Search pages by offset, unlike notifications, which page by key. Results are
ordered by relevance — a property of the query and of the whole index rather
than of any single document — so there is no stable key to resume from: the
same document can legitimately move when another is indexed. An offset is the
honest representation of "continue where the ranking left off". The encoding
stays opaque so it can be replaced when a provider offers something better.

## Providers

`Provider` has four methods: `Name`, `Index`, `Delete`, `Search`. `Index` is an
upsert keyed by Global ID and `Delete` is idempotent, so replaying either
converges rather than failing.

**Memory** is the default. It scans, ranks simply and deterministically, and
exists so that matching, filtering, pagination and above all the authorization
boundary can be tested and exercised end to end without an engine to deploy. A
deployment with no `OPENSEARCH_URL` gets it, so search is always answerable: an
empty index truthfully returns nothing. What never blurs is the difference
between an empty index and an engine that could not be reached — a provider
failure is reported as a 503, because a user told their search found nothing
will act on it.

**OpenSearch** is a skeleton in one specific sense. The request and response
shapes, the bulk NDJSON framing, the error normalization, the resilience policy
and the authorization narrowing are all present and contract-tested against a
fake cluster. What is absent is anything that depends on cluster decisions
nobody has made: index mappings, lifecycle and reindexing. No BSYSTEM
deployment runs OpenSearch, so none of it has been validated against a real
cluster. It is not a stub, though — it builds real requests, so the first
person to point it at a cluster is debugging the cluster rather than this file.

Ranking weights title above summary in both providers, so switching engine
changes relevance quality without changing which field matters.

## Index, update and delete

| Operation | Call |
| --- | --- |
| index or update | `POST /api/service/v1/search/documents` |
| delete | `DELETE /api/service/v1/search/documents/{id}` |

Both require `search.index`, which no human role holds: a person searching does
not index. Indexing is how the platform mirrors an authoritative system into
something more people can read at once, which makes it a machine capability and
an authorization decision at the same time.

An indexer keeps the index current by following the platform's own events. The
mapping is one-directional and deliberately small:

| Event | Operation |
| --- | --- |
| `client.created`, `client.updated` | index `CL-*` |
| `contact.created`, `contact.updated` | index `CT-*` |
| `project.created`, `project.updated` | index `PR-*` |
| `task.created`, `task.updated`, `task.completed` | index `TSK-*` |
| `document.created`, `document.updated` | index `DOC-*` |
| `*.deleted` | delete the Global ID |

The platform does not run that indexer itself. Source systems remain
authoritative, and an indexer that polled them would be a second integration
with its own idea of what a client is. When one is built it belongs alongside
the adapters, reading the normalized API it already has.

Both operations are counted on `bsystem_search_operations_total`, because an
indexing pipeline that has silently stopped looks exactly like one with nothing
to do.
