# Contributing to the Integration Core

## Setup

Go 1.26. Nothing else is required to build or to run the unit tests.

```bash
go build ./...
go test ./...
```

## Before you push

```bash
gofmt -l .        # must print nothing
go vet ./...
go test -race ./...
```

CI additionally runs `staticcheck`, `govulncheck`, Gitleaks, Trivy, CodeQL and
a Spectral lint of `docs/openapi.yaml`. The two most worth running yourself:

```bash
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
npx --yes @stoplight/spectral-cli@6 lint docs/openapi.yaml --fail-severity=error
```

**Run the E2E stack for anything that touches a database query.** There is no
PostgreSQL in the unit test environment, so the store layer's SQL first
executes in that stack — and that is where two real defects have surfaced
rather than locally. See `docs/DEVELOPMENT.md`.

## What CI will catch that you might not expect

`cmd/server/openapi_test.go` compares the contract against the server's own
route inventory. A route added without documentation, a documented operation
with no route, a mismatched authentication boundary, or an error code a handler
emits that the schema does not list — each fails the build. That is
deliberate: callers are told to branch on `code`, and an undocumented one makes
that instruction false.

`internal/repoguard` refuses a compiled executable or an archive tracked in
Git, by leading bytes rather than by filename, and checks that the default
build output of every command under `cmd/` is ignored. `go build ./cmd/x` with
no `-o` writes `./x` in the repository root; this repository carried a 13 MB
binary committed exactly that way, past `.gitignore` rules covering only the
directories a build can be told to use. Add a command and the second test tells
you to add its `/name` line.

Nothing binary is tracked here. If something has to be, add it to the `allowed`
map in that test with the reason — the point is that it argues for itself in a
diff somebody reads, which is the one thing a binary blob otherwise escapes.

## Extending it

`docs/DEVELOPMENT.md` covers adding an endpoint and adding a migration,
including the one ordering rule that is easy to get wrong: **a detail endpoint
refuses before it reads.** Two tests assert that ordering by reading the
handler source, because both orderings look identical to a caller who is
properly authorized — the difference is only visible to a refused one, and what
they see is whether the record existed.

Migrations are additive. There are no down migrations, so a rollback moves the
binary and not the schema.

## Commits

Conventional Commit style, one coherent change per commit:

```text
feat: add normalized client detail endpoint
fix: normalize upstream timeout errors
test: add tenant isolation matrix
docs: document adapter retry policy
ci: add OpenAPI validation
refactor: extract authorization scope evaluator
security: fix a reachable vulnerability
chore: bump the Go toolchain
```

Not `misc changes`, `update files`, `fix stuff`, `wip`.

**The message body is where the reasoning goes.** A diff shows what changed; it
cannot show what else was considered, or what the change is protecting
against. If a commit's body seems long, read a few in the history and then try
reconstructing the same decision from the diff alone.

Never force-push a shared branch. Never rewrite published history.

## Releases and the changelog

This repository publishes no package. The deployable artifact is a container
image built from a commit, so **the commit is the version** and the image is
tagged with its SHA.

There is therefore no `CHANGELOG.md`, and adding one would create a second
history that drifts from the first. The commit history is the changelog, which
is a large part of why commit messages here carry the reasoning rather than a
restatement of the diff. `TASKS.md` records what is done, what is in progress
and what is blocked.

Whether the platform should also publish tagged releases is an owner decision
that has not been made. Nothing depends on it today: every deployment is built
from a known commit.

## The rule that matters most

**Never weaken a check to get a green build.** Not a disabled test, not a
skipped lint rule, not a broadened allow-list, not a lowered severity
threshold.

A check exists because something went wrong once. Turning it off does not
remove the problem; it removes the only thing that would have told you about
the next one. If a check is wrong, fix the check and say why in the commit —
that is a change a reviewer can evaluate, which a silent exemption is not.

A finding that is genuinely a false positive gets the narrowest possible
remedy, scoped so it cannot mask anything else, with the reasoning written
down. There is a worked example in `bsystem-integration-core/.gitleaksignore`.

## When to stop and ask

Some work cannot be finished without a decision only the owner can make:

- real credentials, API keys or passwords
- production deployment, restart, DNS or TLS
- credential rotation
- destructive database operations
- a commercial commitment, such as an SLA target
- customer ownership that nobody has defined yet

For these, record a blocked entry in `TASKS.md` with what is needed, and move
to the next independent task. **A plausible default for one of these is worse
than a blocked task**, because a blocked task is visible and a guess is not:
an invented SLA target appears in front of a customer as a promise, and reads
exactly like a real one.
