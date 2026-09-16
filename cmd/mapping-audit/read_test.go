package main

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The audit is meant to gate a stage acceptance. A tool in that position whose
// queries have never run against a real schema is worth very little: the first
// time anyone learns a column was renamed would be the evening of the
// acceptance, from a tool that was supposed to prevent exactly that.
//
// These tests run its real queries against a real database, with problems
// planted deliberately so that each finding is seen to be produced rather than
// assumed.

func auditDatabase(ctx context.Context, t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect for provisioning: %v", err)
	}
	defer admin.Close()

	name := "bsystem_audit_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
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
	dsn := parsed.String()

	// The audit reads a migrated schema, so build one the same way the
	// platform does rather than hand-writing a subset here: a hand-written
	// schema would keep passing after the real one changed, which is the
	// failure this test exists to catch.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to audit database: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := applyMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate audit database: %v", err)
	}
	return dsn, pool
}

// applyMigrations runs the platform's own migration files. They are embedded
// in internal/platformdb, which this package cannot reach into, so they are
// read from disk at the path the repository guarantees.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := os.ReadDir("../../internal/platformdb/migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	// Filename order is the migration order.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		body, err := os.ReadFile("../../internal/platformdb/migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func TestReadRunsAgainstTheRealSchemaAndFindsNothingWrong(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn, pool := auditDatabase(ctx, t)

	// A small, coherent world: two mapped entities and a server that points at
	// one of them.
	if _, err := pool.Exec(ctx, `
INSERT INTO global_entities (global_id, entity_type, source, source_id)
VALUES ('CL-000001','client','espocrm','acc-1'),
       ('SRV-000001','server','inventory','srv-1')`); err != nil {
		t.Fatalf("seed entities: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO servers (global_id, name, environment, client_id, source, source_id)
VALUES ('SRV-000001','host-1','stage','CL-000001','inventory','srv-1')`); err != nil {
		t.Fatalf("seed server: %v", err)
	}

	input, err := read(ctx, dsn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(input.Mappings) != 2 {
		t.Fatalf("expected two mappings, got %d", len(input.Mappings))
	}
	// The prefixes come from the seeded counters. If a migration stopped
	// registering one, every entity of that type would be reported as
	// unregistered — which is exactly the false alarm that makes people stop
	// reading a tool's output.
	if prefix := input.Prefixes["client"]; prefix != "CL" {
		t.Fatalf("client prefix should be CL, got %q", prefix)
	}

	if findings := Audit(input); len(findings) != 0 {
		t.Fatalf("a coherent world must produce no findings, got %v", kinds(findings))
	}
}

func TestReadDetectsPlantedProblemsEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn, pool := auditDatabase(ctx, t)

	if _, err := pool.Exec(ctx, `
INSERT INTO global_entities (global_id, entity_type, source, source_id)
VALUES
 ('CL-000001','client','espocrm','acc-1'),
 -- the same upstream record mapped twice
 ('PR-000001','project','redmine','7'),
 ('TSK-000001','task','redmine','7'),
 -- a Global ID carrying the wrong prefix for its type
 ('PR-000009','client','espocrm','acc-9'),
 ('SRV-000001','server','inventory','srv-1'),
 ('SRV-000002','server','inventory','srv-2')`); err != nil {
		t.Fatalf("seed entities: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO servers (global_id, name, environment, client_id, source, source_id)
VALUES
 -- points at a client that does not exist
 ('SRV-000001','host-1','stage','CL-404','inventory','srv-1'),
 -- carries no owner at all
 ('SRV-000002','host-2','stage',NULL,'inventory','srv-2')`); err != nil {
		t.Fatalf("seed servers: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO support_records (global_id, kind, title, severity, status, client_id, reported_by)
VALUES ('INC-000001','incident','ownerless','high','new',NULL,'USR-000001')`); err != nil {
		t.Fatalf("seed support record: %v", err)
	}

	input, err := read(ctx, dsn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	found := map[string]int{}
	for _, finding := range Audit(input) {
		found[finding.Kind]++
	}

	for kind, want := range map[string]int{
		"ambiguous_source_mapping":   1, // redmine:7 mapped to two Global IDs
		"invalid_global_id_prefix":   1, // PR-000009 typed as a client
		"missing_referenced_mapping": 1, // SRV-000001 → CL-404
		// Only two: SRV-000002 and the support record. SRV-000001 carries a
		// client_id — a dangling one, reported as a missing reference — and a
		// record pointing at the wrong owner is a different problem from one
		// pointing at none.
		"missing_ownership": 2,
	} {
		if found[kind] != want {
			t.Errorf("expected %d %s finding(s), got %d", want, kind, found[kind])
		}
	}
}

// The tool must not write. This is the property that makes it safe to point at
// a live stage database during an acceptance, and it is asserted rather than
// trusted to the code reading as read-only.
func TestReadWritesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn, pool := auditDatabase(ctx, t)

	if _, err := pool.Exec(ctx, `
INSERT INTO global_entities (global_id, entity_type, source, source_id)
VALUES ('CL-000001','client','espocrm','acc-1')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := snapshot(ctx, pool)
	if err != nil {
		t.Fatalf("snapshot before: %v", err)
	}
	writesBefore := writeCount(ctx, t, pool)

	if _, err := read(ctx, dsn); err != nil {
		t.Fatalf("read: %v", err)
	}
	after, err := snapshot(ctx, pool)
	if err != nil {
		t.Fatalf("snapshot after: %v", err)
	}

	for table, count := range before {
		if after[table] != count {
			t.Errorf("%s changed from %d to %d rows; the audit must not write", table, count, after[table])
		}
	}

	// A stronger statement than counting rows: ask PostgreSQL whether the
	// database recorded any insert, update or delete while the audit ran. A
	// tool that wrote a row and removed it again would pass a row count.
	//
	// Cumulative statistics, not pg_stat_xact_user_tables — that view reports
	// the transaction of the session asking, which is this test's, not the
	// audit's. Reading it here counted this test's own seeding.
	if writes := writesSince(ctx, t, pool, writesBefore); writes != 0 {
		t.Errorf("the database recorded %d row changes while the audit ran", writes)
	}
}

// writeCount reads cumulative insert/update/delete counters for this database.
func writeCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	// Statistics are flushed asynchronously, so a read without this can miss
	// a write that did happen — which would make this test pass for the wrong
	// reason, the worst outcome available to it.
	if _, err := pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	var writes int64
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(SUM(n_tup_ins + n_tup_upd + n_tup_del), 0)
FROM pg_stat_user_tables`).Scan(&writes); err != nil {
		t.Fatalf("read write statistics: %v", err)
	}
	return writes
}

func writesSince(ctx context.Context, t *testing.T, pool *pgxpool.Pool, before int64) int64 {
	t.Helper()
	return writeCount(ctx, t, pool) - before
}

func snapshot(ctx context.Context, pool *pgxpool.Pool) (map[string]int, error) {
	counts := map[string]int{}
	for _, table := range []string{"global_entities", "servers", "support_records", "global_id_counters"} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			return nil, err
		}
		counts[table] = count
	}
	return counts, nil
}

// A database the audit cannot reach must fail with the DSN redacted, because
// the DSN is the one credential this tool holds and a connection error is the
// most likely thing anyone pastes into a ticket.
func TestConnectionFailureDoesNotDiscloseTheDSN(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dsn := "postgres://bsystem:hunter2@127.0.0.1:1/does_not_exist?sslmode=disable&connect_timeout=2"
	_, err := read(ctx, dsn)
	if err == nil {
		t.Fatal("expected a connection failure")
	}
	if redacted := redactDSN(err.Error(), dsn); strings.Contains(redacted, "hunter2") {
		t.Fatalf("the password survived redaction: %q", redacted)
	}
}
