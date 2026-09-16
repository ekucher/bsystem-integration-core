# Migration readiness

An audit of every migration the Integration Core carries, for a stage
deployment against a database that has never run BSYSTEM.

## How migrations are applied

There is no separate migration command. `platformdb.Open` runs `Migrate`
before returning, so **migrations apply on every startup**, in filename order,
from files embedded in the binary. Two consequences follow, and both matter
more than the individual migrations below:

1. **Every migration must be idempotent**, because every one of them runs again
   on every restart. They are written that way — `CREATE TABLE IF NOT EXISTS`,
   `CREATE INDEX IF NOT EXISTS`, `INSERT … ON CONFLICT DO UPDATE`. A migration
   that were not idempotent would not fail at deployment; it would fail at the
   next restart, which is usually at the worst possible moment.
2. **A failed migration prevents startup.** The pool is closed and the process
   exits. The platform never serves traffic against a half-migrated schema —
   which is the right trade, but it means a bad migration is an outage rather
   than a degradation.

Since P17, each applied file is recorded in `schema_migrations` with a SHA-256
of its contents. The ledger is what makes two questions answerable: how far has
this database been taken, and has a file changed since it was applied. The
migrations still re-run regardless — recording is additive and changed no
existing behaviour.

## Order and content

Applied in lexical filename order. The numeric prefix is therefore load-bearing.

| # | File | Adds | Rewrites data | Backward compatible |
| --- | --- | --- | --- | --- |
| 1 | `001_init.sql` | `identities`, `modules`, `global_id_counters`, `global_entities`, `audit_events`; 5 indexes; seeds modules and ID counters | no | n/a — first |
| 2 | `002_service_identities.sql` | `service_identities`, `adapter_registry`; 1 index; seeds the `SVC` counter | no | yes |
| 3 | `003_rbac_scopes.sql` | `roles`, `permissions`, `role_permissions`, `role_modules`, `group_role_mappings`, `principal_scopes`; 1 index; seeds roles, permissions, group mappings | no | yes |
| 4 | `004_notifications.sql` | `notifications`, `notification_reads`; 2 indexes; seeds notification permissions | no | yes |
| 5 | `005_search.sql` | seeds `search.index` permission only | no | yes |
| 6 | `006_operations.sql` | `servers`, `operations_events`; 4 indexes; seeds operations permissions | no | yes |
| 7 | `007_support.sql` | `support_records`, `support_relations`, `support_sla_policies`; 2 indexes | no | yes |
| 8 | `008_ai.sql` | `ai_requests`; 2 indexes; seeds AI permissions | no | yes |
| 9 | `009_indexes.sql` | 4 indexes on existing tables | no | yes |

**No migration drops a table, drops a column, rewrites existing rows, or
performs an irreversible `DELETE`.** Every one is additive. The only statements
that touch existing rows are `ON CONFLICT DO UPDATE` seeds of the platform's own
reference data — roles, permissions, modules, ID counters — which are BSYSTEM's
definitions rather than customer data.

## Runtime duration

Duration is classified, not measured. **No timing here comes from a real
database with real data**, and inventing numbers would be worse than leaving the
question open.

| Category | Migrations | Why |
| --- | --- | --- |
| Trivial | 1–8 on an empty database | `CREATE TABLE` and small seed inserts; work is independent of data volume |
| Data-dependent | 6, 7, 8, 9 on a populated database | Index creation costs scale with the rows already present |

On a first stage deployment every table is empty, so the whole sequence is
trivial. The data-dependent row matters later: `009_indexes.sql` adds indexes to
`audit_events`, `support_records` and `notification_reads`, and `audit_events`
is the table that grows without bound. Re-running it against a large audit table
is the one place in this sequence where a restart could take a noticeable time.

## Locking

`CREATE INDEX` without `CONCURRENTLY` takes a lock that blocks writes to the
table for the duration of the build. On an empty database this is
imperceptible. On a populated one, the indexes in `009_indexes.sql` would block
writes to `audit_events` — meaning audit writes, meaning every authenticated
request that records one.

This is acceptable today and should be revisited before the platform carries
production volume. `CONCURRENTLY` cannot be used here as-is: it may not run
inside a transaction block, and it can leave an invalid index behind on failure,
which an idempotent `IF NOT EXISTS` would then skip forever. That is a design
change, not a tweak, and it is recorded here rather than made quietly.

## Rollback strategy

**There is no down-migration, by design.** Every migration is additive, so the
rollback for a bad *code* deployment is to deploy the previous image: the extra
tables and indexes are inert to code that does not know about them.

What this does not cover is a migration that is itself wrong. For that the
rollback is a database restore, which is why the restore procedure in
`bsystem-deploy/docs/BACKUP-RESTORE.md` is a prerequisite for stage acceptance
rather than a follow-up to it.

Ordering for a rollback:

1. Stop the Integration Core — otherwise it re-applies migrations on restart
   and undoes the rollback.
2. Restore or correct the database.
3. Start the previous image.

## What CI proves

`internal/platformdb/migrate_test.go` runs on every build against a database
created for that test:

- every migration applies to a genuinely **empty** database;
- the recorded level matches what the binary carries;
- applying them a second time is a no-op, which is the restart path;
- the application **starts** against the resulting schema and finds its seeded
  roles — a schema with no roles would deny every request, deny-by-default
  being what it is.

The test creates and drops its own database rather than using the shared CI
one. Every other database test runs against a database that earlier runs have
already migrated, so none of them can detect a migration that only works
because something before it had already created part of the schema.

## Stage checklist

- [ ] `DATABASE_URL` points at a database BSYSTEM owns, not a shared one
- [ ] the role in the DSN may `CREATE TABLE`, `CREATE INDEX` and `CREATE DATABASE`-free DDL on that database
- [ ] a verified backup exists **before** first startup (see the backup runbook)
- [ ] first startup is watched: a migration failure is an exit, not a warning
- [ ] `bsystem_schema_migrations_applied` in `/metrics` reports the expected level afterwards
