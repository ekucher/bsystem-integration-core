-- Module administration (bsystem-hub PR #27 consumer contract).
--
-- Three additive changes to the existing, already-real module registry
-- (modules / role_modules, migrated in 013-017) — no new tables, no new
-- relationship: allowed_roles is the existing role_modules join, read and
-- written from the module's side instead of the role's.

-- 1. `status` becomes a closed vocabulary. The consumer (HUB) only ever
-- sends/expects active|maintenance|disabled; existing rows use the older,
-- unconstrained free-text values ('ready', 'planned') this table started
-- with. Normalize existing data — 'ready' meant "live and launchable", i.e.
-- active; anything else defaults to the safe, invisible state rather than
-- guessing it should be live.
--
-- Deliberately NOT a CHECK constraint, even though the invariant would
-- normally belong in persistence (see the platformdb.ModuleStatuses guard
-- and this migration's own PR description for why a constraint was tried and
-- reverted): 001_init.sql — immutable; editing it changes what an already-
-- applied migration's checksum covers, exactly what
-- TestAnEditedMigrationIsReportedAsDrift exists to catch — re-INSERTs its
-- seven demo modules with status='planned' on every boot via
-- `ON CONFLICT (id) DO UPDATE SET name=...` (status is not in that SET
-- list, so an existing row's status is never actually overwritten by it).
-- PostgreSQL still validates CHECK constraints against the proposed row
-- during ON CONFLICT's speculative insertion, before conflict resolution
-- decides the row will just be updated — so a CHECK here made 001_init.sql
-- fail on every replay after the first, breaking exactly the idempotent-
-- restart guarantee TestMigrationsApplyToAnEmptyDatabase /
-- TestSeveralInstancesCanStartAtOnce exist to prove. The vocabulary is
-- enforced instead at the one layer that actually writes it going forward —
-- CreateAdminModule/UpdateAdminModule, via platformdb.ModuleStatuses — which
-- 001_init.sql's legacy seed rows never go through.
UPDATE modules SET status = 'active', updated_at = now() WHERE status = 'ready';
UPDATE modules SET status = 'disabled', updated_at = now() WHERE status NOT IN ('active', 'maintenance', 'disabled');

ALTER TABLE modules ALTER COLUMN status SET DEFAULT 'disabled';

-- 2. `updated_by` — the admin API's PATCH/POST responses and list need to
-- show who last touched a module (HUB spec: "updated_by / updated_at in the
-- list/detail"). Nullable: rows written by a migration, not a person, have
-- none.
ALTER TABLE modules ADD COLUMN IF NOT EXISTS updated_by TEXT;

-- 3. The new admin permission, following the exact shape identity.user.*
-- already established (011/012_identity_admin*.sql): the platform's own
-- Administrator role gets it, granted explicitly rather than only through
-- the '*' wildcard, so a future role that gains module administration
-- without gaining every permission has something concrete to copy.
INSERT INTO permissions (id, description) VALUES
('module.admin', 'Administer the connected-application module catalog (create, edit, activate, assign role visibility)')
ON CONFLICT (id) DO UPDATE SET description = EXCLUDED.description;

INSERT INTO role_permissions (role_id, permission_id) VALUES
('administrator', 'module.admin')
ON CONFLICT DO NOTHING;
