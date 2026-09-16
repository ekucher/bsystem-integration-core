# Development

## Running the tests

```bash
gofmt -l .        # must print nothing
go vet ./...
go test -race ./...
```

These are what CI runs, plus `staticcheck` and `govulncheck`. Running them
before pushing is worth the minute: a push that turns CI red costs a cycle and
the reviewers' attention.

The contract test that most often catches a genuine mistake is
`cmd/server/openapi_test.go`. It compares `docs/openapi.yaml` against the
server's own route inventory and fails when either side has an endpoint the
other lacks, when a documented authentication boundary disagrees with the
declared one, or when a handler emits an error code the schema does not
document. Adding a route without documenting it does not compile past CI, by
design.

## Running the platform

There is no PostgreSQL in the unit test environment, so the store layer's
queries first execute in the E2E stack. That is not a preference — it is where
two real defects have surfaced — so **run the E2E stack before pushing
anything that touches a query**:

```bash
cd ../bsystem-deploy
docker compose -f docker-compose.e2e.yml up -d --build --wait
E2E_BASE_URL=http://127.0.0.1:8080 go test -C e2e ./...
docker compose -f docker-compose.e2e.yml down -v
```

The stack needs no credentials: it runs four deterministic mock upstreams and
a throwaway database. See `bsystem-deploy/docs/E2E-ENVIRONMENT.md`.

## Adding an endpoint

1. Add it to `routes()` in `cmd/server/routes.go` with its authentication
   boundary and permission. The router applies both from the declaration, so a
   route cannot accidentally be registered unauthenticated.
2. Write the handler.
3. Document it in `docs/openapi.yaml`, including every error code it emits.
4. Run the tests; the contract test will tell you what you missed.

**A detail endpoint refuses before it reads.** A caller who cannot hold the
permission anywhere is refused before anything is looked up — otherwise they
can tell an existing record from a missing one by the status code and
enumerate identifiers. A scope-confined caller is exempt from that early
refusal, since its authority is per-resource, and is told "not found" when
denied. Copy the shape from `resolveScopedEntity`; two tests assert the
ordering by reading the handler source, because both orderings look identical
to a caller who is properly authorized.

## Adding a migration

Migrations live in `internal/platformdb/migrations/` and run in filename
order at startup. They must be **additive**: the platform has no down
migrations, and a deployment can be rolled back to the previous binary while
the schema stays ahead of it.

Never drop a column or a table autonomously. Adding one that nothing reads yet
is free; removing one that something still reads is an outage.

## Conventions worth knowing before you argue with them

Each of these is written up where it was decided, because each looks arbitrary
until you know what it is avoiding:

| Convention | Where |
| --- | --- |
| Cursors are opaque and the encoding varies by collection | `adr/ADR-007` |
| Errors are normalized and callers branch on `code` | `adr/ADR-008` |
| Upstream failures are retried, bounded, and circuit-broken | `adr/ADR-006` |
| Unknown customer ownership denies rather than discloses | `adr/ADR-005` |
| Search authorizes every result individually | `adr/ADR-009` |
| The AI gateway never widens its own context | `adr/ADR-010` |

## Commits

Conventional Commit style. One coherent change per commit.

The commit message is where the reasoning goes. A diff shows what changed; it
cannot show what else was considered, or what the change is protecting
against. Several commits in this repository exist mainly to record why
something is the way it is — if that seems excessive, read
`adr/ADR-005` and then try to reconstruct it from the diff alone.
