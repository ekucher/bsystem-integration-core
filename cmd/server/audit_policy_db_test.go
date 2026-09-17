package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Audit durability policy, executed.
//
// The platform has two audit behaviours and the difference between them is a
// security decision, not an implementation detail. An authorization change
// that happened without a record cannot be reconstructed from anything
// afterwards: the platform keeps no other trace of who granted whom what. A
// notification being marked read can be inferred from the notification itself.
//
// So the first is fail-closed and the second is fail-open, and both halves are
// proved here — including the second, because "fail-closed" applied to
// everything would take the platform down whenever the audit table was
// unwell, which is its own kind of failure.

// breakAuditWrites makes every INSERT into audit_events fail, deterministically
// and for this database only.
//
// A trigger rather than a dropped table: the audit table has to still exist,
// because the point is to fail the write rather than to fail the statement
// before it reaches the table. It is removed when the test ends so a shared
// fixture is not left poisoned.
func breakAuditWrites(t *testing.T, application *app) {
	t.Helper()
	ctx := context.Background()
	pool := auditPool(t, application)
	statements := []string{
		`CREATE OR REPLACE FUNCTION bsystem_test_refuse_audit() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'audit sink is unavailable'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER bsystem_test_refuse_audit BEFORE INSERT ON audit_events
		 FOR EACH ROW EXECUTE FUNCTION bsystem_test_refuse_audit()`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("install the audit failure: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS bsystem_test_refuse_audit ON audit_events`)
	})
}

// auditPool opens a second connection to the same database the application
// uses, so the test can change the schema without reaching into the store's
// internals.
func auditPool(t *testing.T, application *app) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDSN(t, application))
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The fail-closed half. A grant whose audit record cannot be written must not
// take effect, and the caller must be told it did not — an administrator who
// believes a grant succeeded will not make it again.
func TestAScopeGrantIsRefusedWhenItsAuditRecordCannotBeWritten(t *testing.T) {
	application, handler := integrationApp(t, map[string]map[string]any{
		"admin": principal("audit-admin", "BSYSTEM-Admins"),
	})
	if recorder := call(t, handler, http.MethodGet, "/api/v1/me", "admin"); recorder.Code != http.StatusOK {
		t.Fatalf("sign in: status = %d", recorder.Code)
	}

	breakAuditWrites(t, application)

	grant := map[string]any{
		"principal_type": "user", "principal_id": "USR-000999",
		"scope_type": "client", "scope_id": "CL-000001",
		"permission_id": "crm.client.read",
	}
	recorder := post(t, handler, "/api/v1/admin/rbac/scopes", "admin", grant)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a grant whose audit record failed was reported as successful", recorder.Code)
	}
	var failure map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode refusal: %v (body: %s)", err, recorder.Body.String())
	}
	if failure["code"] != "audit_unavailable" {
		t.Errorf("code = %q, want audit_unavailable: the administrator cannot tell this from a store outage", failure["code"])
	}
	// The refusal must not describe the database. This endpoint decides who
	// may see what, and a raw error here names tables and constraints.
	for _, secret := range []string{"audit_events", "trigger", "relation", "SQLSTATE", "plpgsql"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Errorf("the refusal names %q: %s", secret, recorder.Body.String())
		}
	}

	// And the grant must not exist. This is the assertion the whole policy is
	// about: a 503 that left the scope granted would be worse than a 201,
	// because nobody would go looking for it.
	listing := call(t, handler, http.MethodGet, "/api/v1/admin/rbac/scopes?principal_type=user&principal_id=USR-000999", "admin")
	if listing.Code != http.StatusOK {
		t.Fatalf("list scopes: status = %d (body: %s)", listing.Code, listing.Body.String())
	}
	if strings.Contains(listing.Body.String(), "CL-000001") {
		t.Errorf("the scope was granted although its audit record failed: %s", listing.Body.String())
	}
}

// The fail-open half, and the reason it is not an oversight. A platform that
// refuses every request when the audit table is unwell has turned a
// bookkeeping failure into an outage. These paths describe something the
// platform keeps its own record of, so the request stands and the failure is
// counted where an alert can see it.
func TestALowRiskPathStillWorksWhenTheAuditSinkIsBroken(t *testing.T) {
	application, handler := integrationApp(t, map[string]map[string]any{
		"admin": principal("audit-open", "BSYSTEM-Admins"),
	})
	if recorder := call(t, handler, http.MethodGet, "/api/v1/me", "admin"); recorder.Code != http.StatusOK {
		t.Fatalf("sign in: status = %d", recorder.Code)
	}
	// Allocate before breaking audit, so the read below has something to read.
	created := post(t, handler, "/api/v1/global-ids", "admin", map[string]any{
		"entity_type": "client", "source": "audit-test", "source_id": "open-1",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("allocate: status = %d (body: %s)", created.Code, created.Body.String())
	}
	var entity struct {
		GlobalID string `json:"global_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &entity); err != nil {
		t.Fatalf("decode: %v", err)
	}

	breakAuditWrites(t, application)

	// global_id.read is audited and fail-open. The read must still answer.
	read := call(t, handler, http.MethodGet, "/api/v1/global-ids/"+entity.GlobalID, "admin")
	if read.Code != http.StatusOK {
		t.Errorf("reading a Global ID while the audit sink is broken: status = %d, want 200 (body: %s)", read.Code, read.Body.String())
	}
	// So must an ordinary authenticated request that audits nothing, which is
	// the wider claim: a broken audit table is not an outage.
	if profile := call(t, handler, http.MethodGet, "/api/v1/me", "admin"); profile.Code != http.StatusOK {
		t.Errorf("GET /api/v1/me while the audit sink is broken: status = %d, want 200", profile.Code)
	}

	// The failure is observable rather than only logged.
	metrics := call(t, handler, http.MethodGet, "/metrics", "")
	if !strings.Contains(metrics.Body.String(), `bsystem_audit_writes_total{action="global_id.read",outcome="failed"}`) {
		t.Error("an audit write failure was not counted; a dashboard cannot tell a broken audit trail from a quiet week")
	}
}
