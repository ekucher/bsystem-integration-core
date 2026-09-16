package platformdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationDatabase creates a throwaway database and returns a DSN for it.
//
// A fresh database, not a fresh schema: migrations create extensions, types
// and search paths that a schema-scoped run would silently resolve against
// whatever the shared test database already had. The point of this test is
// that an empty database works, so anything inherited defeats it.
func migrationDatabase(ctx context.Context, t *testing.T, adminDSN string) string {
	t.Helper()

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect for provisioning: %v", err)
	}
	defer admin.Close()

	name := "bsystem_migrate_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pool, err := pgxpool.New(cleanupCtx, adminDSN)
		if err != nil {
			return
		}
		defer pool.Close()
		_, _ = pool.Exec(cleanupCtx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

// TestMigrationsApplyToAnEmptyDatabase is the test that a first stage
// deployment depends on. Every other database test runs against a database
// that previous runs have already migrated, so none of them would notice a
// migration that only works because something earlier had created part of it.
func TestMigrationsApplyToAnEmptyDatabase(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsn := migrationDatabase(ctx, t, adminDSN)

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open and migrate an empty database: %v", err)
	}
	defer db.Close()

	state, err := db.SchemaLevel(ctx)
	if err != nil {
		t.Fatalf("read schema level: %v", err)
	}
	embedded, err := EmbeddedSchemaLevel()
	if err != nil {
		t.Fatalf("read embedded level: %v", err)
	}
	if state.Applied != embedded.Applied {
		t.Fatalf("applied %d migrations, binary carries %d", state.Applied, embedded.Applied)
	}
	if state.Level != embedded.Level {
		t.Fatalf("applied level %q, binary carries %q", state.Level, embedded.Level)
	}

	// Migrations run on every startup, so being idempotent is not a nicety:
	// a migration that fails the second time takes the platform down on its
	// next restart rather than at deployment, when nobody is watching.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-applying migrations must be a no-op: %v", err)
	}

	after, err := db.SchemaLevel(ctx)
	if err != nil {
		t.Fatalf("read schema level after re-apply: %v", err)
	}
	if after.Applied != state.Applied {
		t.Fatalf("re-applying changed the recorded count: %d then %d", state.Applied, after.Applied)
	}
}

// The application must start against the schema its own migrations produce.
// Startup does more than migrate — it seeds, reads roles and opens a pool —
// and a seed that depends on a table a later migration creates would pass the
// migration test and still fail to boot.
func TestApplicationStartsAgainstAFreshlyMigratedSchema(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsn := migrationDatabase(ctx, t, adminDSN)

	first, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first startup: %v", err)
	}
	first.Close()

	// The second Open is the restart. It exercises the path that every
	// deployed instance takes on every restart for the rest of its life.
	second, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("restart against an already migrated schema: %v", err)
	}
	defer second.Close()

	if err := second.Ping(ctx); err != nil {
		t.Fatalf("ping after restart: %v", err)
	}

	roles, err := second.ListRoles(ctx)
	if err != nil {
		t.Fatalf("read seeded roles: %v", err)
	}
	if len(roles) == 0 {
		t.Fatal("a freshly migrated database must carry the seeded roles; deny-by-default with no roles denies everyone")
	}
}

// An edited migration must stay visible for as long as the divergence lasts.
//
// Migrations are re-applied on every startup and are written to be idempotent,
// so editing one that a database has already applied does not fail: the file
// runs, most of its statements no-op, and whatever the edit added is silently
// absent from that database while a database migrated after the edit has it.
// Nothing about the filename, the level or the applied count differs, so two
// deployments report an identical schema version while holding different
// schemas — and that is the one question the ledger exists to answer.
//
// The ledger recorded a checksum and then overwrote it with the new file's on
// the next startup, destroying the evidence at exactly the moment it became
// worth having. This test pins that the first checksum survives and that the
// divergence is reported.
func TestAnEditedMigrationIsReportedAsDrift(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsn := migrationDatabase(ctx, t, adminDSN)

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first startup: %v", err)
	}
	defer db.Close()

	state, err := db.SchemaLevel(ctx)
	if err != nil {
		t.Fatalf("read schema level: %v", err)
	}
	if state.Applied == 0 {
		t.Fatal("a migrated database records no migration; the rest of this test would pass vacuously")
	}
	if len(state.Drifted) != 0 {
		t.Fatalf("a freshly migrated database reports drift: %v", state.Drifted)
	}

	// The migrations are embedded in the binary, so an edit cannot be made on
	// disk from here. Recording a different hash for what was applied is the
	// same condition from the other side: the file the binary carries no
	// longer hashes to what this database applied.
	const edited = "003_rbac_scopes.sql"
	const wasApplied = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := db.pool.Exec(ctx,
		`UPDATE schema_migrations SET checksum=$1 WHERE name=$2`, wasApplied, edited); err != nil {
		t.Fatalf("simulate an edited migration: %v", err)
	}

	// The restart is where the old code lost it. Migrate runs, rewrites the
	// checksum from the file, and the database that had diverged now claims
	// it never did.
	restarted, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer restarted.Close()

	state, err = restarted.SchemaLevel(ctx)
	if err != nil {
		t.Fatalf("read schema level after restart: %v", err)
	}
	if len(state.Drifted) != 1 || state.Drifted[0] != edited {
		t.Errorf("drifted = %v, want exactly [%s]; a restart must not clear the record of a divergence", state.Drifted, edited)
	}

	// The evidence itself, not just the verdict. Without the original hash
	// nobody can tell which of the two contents the database actually holds.
	var recorded string
	if err := restarted.pool.QueryRow(ctx,
		`SELECT checksum FROM schema_migrations WHERE name=$1`, edited).Scan(&recorded); err != nil {
		t.Fatalf("read recorded checksum: %v", err)
	}
	if recorded != wasApplied {
		t.Errorf("recorded checksum = %q, want the one this database applied; overwriting it destroys the only record of what ran here", recorded)
	}

	// Drift is specific to the file that moved. Reporting every migration
	// would be the same as reporting none: nobody would read the list.
	if state.Applied < 2 {
		t.Fatal("only one migration recorded; the specificity check below proves nothing")
	}
}

// Several instances starting at once must all come up.
//
// Migrations are written to be idempotent, which makes them safe to re-run and
// says nothing about running them simultaneously. CREATE TABLE IF NOT EXISTS
// is not atomic against another session creating the same table: both pass the
// existence check and one loses on an internal unique index. Migrate runs from
// Open, so that is a failure to start.
//
// Measured before the lock, four instances released together against a fresh
// database: three did not boot, each with
//
//	create schema history: ERROR: duplicate key value violates unique
//	constraint "pg_type_typname_nsp_index" (SQLSTATE 23505)
//
// This is the ordinary shape of a deployment — several replicas, a rolling
// restart, or everything returning at once after an outage — and a restart
// policy turns it into flapping rather than reporting what happened.
func TestSeveralInstancesCanStartAtOnce(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dsn := migrationDatabase(ctx, t, adminDSN)

	const instances = 4
	var start sync.WaitGroup
	start.Add(1)
	var finished sync.WaitGroup
	errs := make([]error, instances)

	for i := range instances {
		finished.Add(1)
		go func(i int) {
			defer finished.Done()
			start.Wait() // release them together, or they migrate in turn
			db, err := Open(ctx, dsn)
			errs[i] = err
			if db != nil {
				db.Close()
			}
		}(i)
	}
	start.Done()
	finished.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d did not start: %v", i, err)
		}
	}

	// And the schema is whole rather than partly built by whichever instance
	// got furthest. The ledger is the cheapest way to ask.
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open after the concurrent start: %v", err)
	}
	defer db.Close()

	state, err := db.SchemaLevel(ctx)
	if err != nil {
		t.Fatalf("read schema level: %v", err)
	}
	embedded, err := EmbeddedSchemaLevel()
	if err != nil {
		t.Fatalf("read embedded level: %v", err)
	}
	if state.Applied != embedded.Applied || state.Level != embedded.Level {
		t.Errorf("schema is at %s (%d applied), the binary carries %s (%d)",
			state.Level, state.Applied, embedded.Level, embedded.Applied)
	}
	if len(state.Drifted) != 0 {
		t.Errorf("a concurrently migrated database reports drift: %v", state.Drifted)
	}
}
