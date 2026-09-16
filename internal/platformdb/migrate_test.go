package platformdb

import (
	"context"
	"net/url"
	"os"
	"strings"
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
