# Rate limits

What bounds how often one principal may reach an expensive surface, and what
this deliberately does not do.

## The classes

Derived from the route table in `routes.go`, which is already the
authoritative inventory of what the platform serves. Four, because four things
cost differently.

| Class | Surface | Per minute | Burst |
| --- | --- | --- | --- |
| `human` | the API a person's browser calls | `RATE_LIMIT_HUMAN_PER_MINUTE` (300) | `RATE_LIMIT_HUMAN_BURST` (60) |
| `service` | the machine API | `RATE_LIMIT_SERVICE_PER_MINUTE` (1200) | `RATE_LIMIT_SERVICE_BURST` (200) |
| `ai` | `POST /api/v1/ai/ask` | `RATE_LIMIT_AI_PER_MINUTE` (20) | `RATE_LIMIT_AI_BURST` (5) |
| `search` | `GET /api/v1/search` | `RATE_LIMIT_SEARCH_PER_MINUTE` (120) | `RATE_LIMIT_SEARCH_BURST` (30) |

A zero in either column disables that class. The defaults are generous on
purpose: a limit exists to bound abuse, and a limit a legitimate session meets
is an outage the platform inflicted on itself. The AI class is the tight one,
because one request there costs a model call and a fan-out of authorized reads
rather than a query.

A route added to the table falls into a class from its authentication kind, so
a new endpoint is limited by default rather than unlimited until somebody
remembers it. `TestEveryAuthenticatedRouteFallsIntoAClass` enforces that.

**The operational endpoints are not limited.** A limiter on `/metrics` stops the
scrape during exactly the incident the scrape exists for, and one on `/readyz`
makes an orchestrator kill a healthy container.

## What the limit is counted against

A principal the platform has already authenticated: a `USR-*` or a `SVC-*`.

Never a header, a username or a client address. A limit keyed on something the
caller controls is a limit the caller steps around by changing it, and one
keyed on an address throttles everybody behind a shared egress together —
which in practice means one customer's office.

The middleware runs **after** authentication, because that is when the
principal exists, and **before** authorization, because refusing early is the
cheaper half of the point.

## The refusal

```
HTTP/1.1 429 Too Many Requests
Retry-After: 4

{"error": "too many requests", "code": "rate_limited", "request_id": "..."}
```

`Retry-After` is in seconds, always at least 1 — a caller told to wait for
nothing retries immediately and is refused again — and bounded by a full
refill, because a bound longer than that is a caller giving up rather than
backing off.

The body names no principal, no policy and no internal detail. It goes back to
whoever sent the request.

## Metrics

```
bsystem_rate_limit_decisions_total{class="search",outcome="rejected"}
```

Two labels, and deliberately only two. A principal is one time series per
person or per machine identity; a username, an email or a client address would
put *who is being throttled* into a metrics store that is read far more widely
than the audit trail.

Which principal is being limited is answerable from the logs and the audit
trail. How much of each surface is being refused is answerable only from here,
and that is the question during an incident.

## Per instance, and why there is no Redis

The limiter is in memory, per Integration Core process. A deployment running
three instances enforces each limit three times over rather than once in total.

That is a real limitation and it is the deliberate choice. A shared counter in
Redis would make the limit exact, and it would add a dependency whose outage
has to be answered: fail open and the limit is gone at the moment it is most
likely to be needed, fail closed and the platform is gone. Nothing in this
deployment has demonstrated the need for an exact limit, and an approximate one
enforced n times still bounds abuse by a factor of n.

**Distributed rate limiting stays an explicit future decision**, to be taken
when something demonstrates the need — a shared limit that must be exact for a
commercial reason, or a replica count where n× is no longer a bound worth
having. It is not to be added because it is the usual answer.

The bucket table is bounded (10,000 keys, released after 10 minutes idle), so a
platform whose principals are machine identities minted per job does not grow a
bucket per job and release none. The cost of the bound is that a very busy
platform may forget a bucket early and that principal starts full — the limit
is approximate by design, and this is one of the ways.
