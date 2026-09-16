# Performance

Every number here was measured. Nothing in this document is an estimate, a
target, or a figure carried over from another system — if a measurement is
missing, it says so rather than filling the gap.

## What was measured, and where

```text
goos: linux  goarch: amd64
cpu: Intel(R) Xeon(R) Processor @ 2.80GHz, 4 cores
go test -run xxx -bench . -benchtime 300x
```

**This is a CI-class container, not production hardware.** The numbers are
useful as a shape and a baseline to compare against after a change; they are
not a capacity plan, and treating them as one would be inventing the thing
this document refuses to invent.

| Benchmark | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| `Redact` — ordinary text | 4,072 | 576 | 3 |
| `Redact` — text containing credentials | 1,013 | 304 | 7 |
| `BuildPrompt` — 20 fragments | 57,637 | 16,576 | 110 |
| in-memory search — 1,000 documents | 995,666 | 532,713 | 6,006 |
| in-memory search — 10,000 documents | 11,847,707 | 5,283,530 | 60,006 |
| cursor encode + decode | 134 | 16 | 3 |

### The two numbers worth reading

**Redaction is slower on clean text than on text containing a credential.**
That is not backwards. A line with a credential key matches early and returns;
a clean line is scanned word by word to see whether anything *looks* like a
generated secret. The cost is paid on every request, so it belongs on the
request path's budget — 4µs for a fragment is not a concern, and the number
exists so that a future change making it 400µs is visible.

**The in-memory search provider scans, and it costs about 1.2µs per indexed
document.** At 10,000 documents a search is ~12ms and allocates 5MB. That is
the number that says when a deployment has outgrown the default provider, and
it is why the OpenSearch skeleton exists. It is not a defect: the in-memory
provider's job is to make the authorization boundary testable without an
engine to deploy, and it does that at any size a test uses.

## What was fixed, and why it was a problem

Each of these was found by reading the code against its own query and request
paths, not by profiling — which is why they are listed with the reasoning
rather than with a before-and-after figure.

### The listing endpoints were N+1

Every collection handler called `CreateGlobalEntity` once per row — a
transaction per item, and **two** per item for contacts and issues, which also
map their owner. A page of fifty contacts cost a hundred round trips to render
fifty lines, and the cost grew with the page size rather than with the work.

They now batch: one `SELECT` resolves everything already mapped, and only
genuinely new records are allocated. The allocation still goes through
`CreateGlobalEntity`, so there remains exactly one place where an identifier
comes into existence.

The slow path now runs once per record in the platform's lifetime instead of
once per request.

### `ListRoles` was N+1

Two queries per role. With eight roles that is sixteen queries to render a
list nobody would call slow — which is precisely the shape that looks fine
now and does not stay fine.

### Adapter health probes ran in sequence, holding a lock

A readiness check cost the **sum** of every adapter's timeout. Three adapters
at ten seconds each made a thirty-second health endpoint — which a load
balancer times out on, exactly when something is wrong and the answer matters
most.

Worse, the registry's read lock was held across those network calls, so one
unreachable upstream blocked every other reader of the registry for the length
of its timeout.

Probes now run concurrently with a bounded slot count, and the lock is held
only long enough to snapshot the registry. Two tests assert both properties by
blocking a probe and checking that the others start and that `List` does not
block.

### The write timeout was shorter than the AI bound

`WriteTimeout` was 15s while the AI gateway allowed a provider 30s. An AI
request could not complete: the server would cut the response while the
handler was still producing it, and the caller would see a truncated body
rather than the timeout that caused it.

`WriteTimeout` is now derived from the AI bound rather than written as a
constant somebody has to remember to keep in step.

### Nothing shut down gracefully

`ListenAndServe` was wrapped in `log.Fatal`. On SIGTERM the process died
immediately, dropping every request in flight — so a rolling deploy produced a
burst of failures for users who did nothing but arrive at the wrong moment,
and it looked like an intermittent platform fault rather than a deployment.

The server now drains on SIGTERM within a grace period shorter than the
runtime's own kill delay, and says so in the log when a request outlives it. A
silent close looks like a network fault to whoever was holding the connection.

### The connection pool was unconfigured

pgx sizes a pool at `max(4, NumCPU)`, which is a reasonable guess about the
client and no guess at all about the server. PostgreSQL's `max_connections` is
the real limit and is shared with every other client, so a platform sizing its
pool by its own core count will, on a larger machine, quietly take a share it
was never allocated — and fail everyone at once when it runs out.

The pool now has explicit bounds, all overridable per deployment, plus a
connection lifetime so a long-lived pool cannot pin itself to a database
instance that has since been replaced behind a load balancer.

## What is not measured

**No end-to-end throughput or latency figure exists**, and none is quoted.
Producing one that means anything needs production-class hardware, a realistic
data volume and real upstreams — and BSYSTEM has none of the three. The E2E
stack answers correctness questions, not capacity ones; a number from it would
describe four mock upstreams on a CI runner.

`bsystem-deploy/loadtest/` drives the E2E stack concurrently and reports
latency percentiles. It is a **regression harness**: run it before and after a
change and compare. Its absolute numbers describe the mocks.

The index audit in migration `009` records which indexes were added, and also
the candidates that were rejected because the coverage already existed — a
redundant index is not free.
