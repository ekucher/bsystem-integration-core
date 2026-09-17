# Versioning and compatibility

What a release of the Integration Core promises, what counts as a breaking
change, and which of those rules a machine actually checks.

The last part matters most. A compatibility policy nothing enforces is a
description of intentions, and the sections below say for each rule whether it
is checked or only written down.

## The HTTP API

Two surfaces, versioned in the path, and deliberately separate:

```
/api/v1/*           human API — a person's browser, through the HUB
/api/service/v1/*   machine API — a service identity
```

A version in the path is what lets both live at once. **The version changes
only for a breaking change**, and a `v2` would be served beside `v1` rather
than replacing it, because a consumer this platform does not control cannot be
redeployed in step with it.

### Additive, and therefore not a version change

- a new endpoint;
- a new **optional** request field, or a new query parameter with a default
  that preserves current behaviour;
- a new field in a response body;
- a new value in an enum a client only ever *reads*;
- a new error `code` for a condition that previously produced a generic error
  of the same HTTP status.

A consumer that ignores what it does not recognise is unaffected by all of
these. That is the contract the HUB holds up its end of.

### Breaking, and therefore a version change

- removing or renaming an endpoint, a path parameter, a request field or a
  response field;
- changing the type or the meaning of an existing field;
- making an optional request field required;
- narrowing what is accepted — a new validation rule that refuses input that
  used to work;
- changing the HTTP status for an existing condition;
- changing an error `code`, which is the part of an error body clients switch
  on. The `error` text is prose and may change freely; the `code` may not;
- changing an identifier's shape. Global IDs are immutable and their prefixes
  are part of the contract.

**Tightening authorization is not a breaking change.** A caller who could reach
something they should not, and now cannot, is a defect being fixed. It is
announced in the release notes and does not move the version.

### What is checked

| Rule | Checked by |
| --- | --- |
| every served route is documented, and every documented operation is served | `TestEveryRouteIsDocumented` / `TestEveryDocumentedOperationIsServed` |
| every error `code` a handler emits is documented | `TestEveryEmittedErrorCodeIsDocumented` |
| the HUB asks only for paths the platform serves | `bsystem-hub/scripts/check-api-contract.py` |
| that gate still catches a breaking change | `bsystem-hub/scripts/tests/contract-gate.test.sh` |
| the metrics exposition keeps its series and labels | `TestTheMetricsEndpointKeepsItsContract` |

**Not checked:** field-level additive-versus-breaking. Nothing compares this
release's schemas against the previous release's, so removing a response field
passes CI. That is a real gap; it is written here rather than implied by the
absence of a check.

## OpenAPI

`docs/openapi.yaml` is the **source of truth for the contract**, and the
implementation is tested against it rather than generated from it. The
direction matters: a document generated from the code cannot disagree with the
code, which makes it useless as a check.

Its `info.version` tracks the contract, not the build. It moves when the
contract changes and stays still for a release that changes only behaviour.

The release manifest records a **hash** of the document rather than that
version, because a hash answers the question a client actually has: is the
contract I generated from the contract being served? Two builds whose
documents hash the same serve the same contract, whatever the version field
says.

There is no runtime endpoint reporting the contract hash, and there does not
need to be: `bsystem_build_info` carries the **commit**, and the commit names
the exact document. Embedding a second copy in the binary would create two
places that can disagree.

## The database schema

Migrations are numbered, forward-only and additive by policy.

- **A release supports the schema level it embeds, and every earlier one it can
  migrate forward from.** `TestUpgradeWorksFromEveryHistoricalSchemaLevel`
  proves the upgrade from each historical level, so "every earlier one" is
  executed rather than assumed.
- **There is no downward migration.** A release does not know how to undo the
  schema of a later one.
- **Running a newer database under an older Core is not supported.** It usually
  works, because the schema is additive — and "usually" is not a support
  statement.

`bsystem_schema_migrations_applied` reports the level the connected database is
actually at, and `bsystem_build_info` carries the level the binary embeds.
They disagree exactly when a database is behind its code, which is the state
that explains an otherwise inexplicable failure after a deployment.
`bsystem_schema_migrations_drifted` reports an edited migration, which is
invisible to both.

### Emergency rollback

**A release containing only additive migrations can be rolled back by
redeploying the previous image.** The extra columns and tables stay; the older
code does not read them. That is the normal case and it is why the migrations
are additive.

**A release containing a destructive migration cannot be rolled back by
redeploying.** Restoring the previous code against a database that has had a
column dropped or a type narrowed means restoring the database too, from a
backup taken before the migration — which loses everything written since.

So, before any release that drops or rewrites:

1. it is a two-release change, never one. Release A stops using the column;
   Release B, after A is accepted, removes it. Between them a rollback is a
   redeploy.
2. take a backup immediately before B, and know its restore time.
3. write the rollback plan down before deploying, not after something fails.

No irreversible migration exists in this platform today, and none was added to
write this section.

## HUB ↔ Core

The HUB declares **no version range**, on purpose. A matrix nothing tests is a
promise nobody can keep.

What is declared instead is a **validated pair**: every green CI run records
the exact Core commit the HUB was checked against, and the release manifest
records the four repository commits a release candidate is made of. The claim
is "these two commits were verified together", which is true and checkable,
rather than "this HUB works with Core 1.2 through 1.7", which nothing has
tried.

A deployment upgrades both from the same manifest. Where that is impossible,
the path gate is the only mechanical guarantee, and it covers paths — not
fields, not error codes.
