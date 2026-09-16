package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// These tests exercise the real routes, the real authentication middleware and
// the real RBAC data together, against a real database. They sit between the
// store tests, which prove a query is right, and the E2E stack, which proves
// the whole deployment works — and they cover the part neither does: that the
// authorization *decisions* the tenant isolation matrix describes are the ones
// the server actually makes.
//
// The matrix is a document. Until something executes it, it is a description
// of intent.

// identityProvider stands in for authentik's UserInfo endpoint. A token is
// simply the name of a fixture, so a test reads as "sign in as a developer"
// rather than as OIDC plumbing.
func identityProvider(t *testing.T, principals map[string]map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		info, ok := principals[token]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	}))
	t.Cleanup(server.Close)
	return server
}

func integrationApp(t *testing.T, principals map[string]map[string]any) (*app, http.Handler) {
	t.Helper()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect for provisioning: %v", err)
	}
	name := "bsystem_api_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	admin.Close()
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

	db, err := platformdb.Open(ctx, parsed.String())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)

	provider := identityProvider(t, principals)
	t.Setenv("AUTHENTIK_USERINFO_URL", provider.URL)

	application := &app{
		db:             db,
		authz:          authz.New(db, authz.DefaultConfinedRoles()),
		searchProvider: searchProvider(),
		aiProvider:     aiProviderFromEnv(),
	}
	return application, application.handler()
}

func call(t *testing.T, handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func principal(subject string, groups ...string) map[string]any {
	return map[string]any{
		"sub": subject, "email": subject + "@example.invalid",
		"preferred_username": subject, "groups": groups,
	}
}

// The actors from the isolation matrix, as tokens.
func matrixPrincipals() map[string]map[string]any {
	return map[string]map[string]any{
		"administrator": principal("admin-1", "BSYSTEM-Admins"),
		"manager":       principal("manager-1", "BSYSTEM-Managers"),
		"developer":     principal("developer-1", "BSYSTEM-Developers"),
		"qa":            principal("qa-1", "BSYSTEM-QA"),
		"devops":        principal("devops-1", "BSYSTEM-DevOps"),
		"customer":      principal("customer-1", "BSYSTEM-Customers"),
		"unmapped":      principal("nobody-1"),
	}
}

// Every authenticated endpoint refuses an anonymous caller. This is the check
// the smoke runner makes against a real deployment; here it runs on every
// route rather than the handful the runner samples.
func TestEveryAuthenticatedRouteRejectsAnonymousCallers(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	for _, r := range routes() {
		if r.Auth == authNone {
			continue
		}
		path := strings.ReplaceAll(r.Path, "{id}", "CL-000001")
		recorder := call(t, handler, r.Method, path, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d to an anonymous caller, want 401", r.Method, r.Path, recorder.Code)
		}
	}
}

func TestOperationalEndpointsAnswerWithoutAToken(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	for _, path := range []string{"/health", "/readyz", "/metrics"} {
		if recorder := call(t, handler, http.MethodGet, path, ""); recorder.Code != http.StatusOK {
			t.Errorf("%s answered %d without a token, want 200", path, recorder.Code)
		}
	}
}

// The matrix, executed. Each row is a permission decision the platform makes
// from its seeded RBAC data, and the expectations here are the document's.
func TestCollectionAccessMatchesTheIsolationMatrix(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	// 403 means refused for want of a permission. 503 means the permission
	// check passed and the request reached an adapter that is not configured
	// here — which is the point: authorization is decided before any upstream
	// is consulted, so a missing integration and a missing permission are
	// distinguishable from the outside.
	for _, probe := range []struct {
		actor, path string
		want        int
	}{
		{"administrator", "/api/v1/clients", http.StatusServiceUnavailable},
		{"administrator", "/api/v1/servers", http.StatusOK},
		{"manager", "/api/v1/clients", http.StatusServiceUnavailable},
		{"manager", "/api/v1/projects", http.StatusServiceUnavailable},
		{"developer", "/api/v1/projects", http.StatusServiceUnavailable},
		{"developer", "/api/v1/clients", http.StatusForbidden},
		{"qa", "/api/v1/projects", http.StatusServiceUnavailable},
		{"qa", "/api/v1/clients", http.StatusForbidden},
		{"qa", "/api/v1/servers", http.StatusForbidden},
		{"devops", "/api/v1/servers", http.StatusOK},
		{"devops", "/api/v1/projects", http.StatusForbidden},
		{"customer", "/api/v1/projects", http.StatusForbidden},
		{"customer", "/api/v1/servers", http.StatusForbidden},
		{"unmapped", "/api/v1/clients", http.StatusForbidden},
		{"unmapped", "/api/v1/projects", http.StatusForbidden},
	} {
		recorder := call(t, handler, http.MethodGet, probe.path, probe.actor)
		if recorder.Code != probe.want {
			t.Errorf("%s GET %s answered %d, want %d (body %s)",
				probe.actor, probe.path, recorder.Code, probe.want, strings.TrimSpace(recorder.Body.String()))
		}
	}
}

// The AI gateway is denied to every role including Manager, because ai.query
// is granted to none of them. Writing this down as a test means the day
// somebody grants it, they do so deliberately.
func TestTheAIGatewayIsDeniedToEveryRoleButTheAdministrator(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	for _, actor := range []string{"manager", "developer", "qa", "devops", "customer", "unmapped"} {
		recorder := call(t, handler, http.MethodPost, "/api/v1/ai/ask", actor)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s reached the AI gateway with %d; ai.query is granted to no role", actor, recorder.Code)
		}
	}
}

// Notifications and search require no permission: they are filtered by
// audience and by what the caller can already reach. An unmapped principal
// therefore gets an empty list rather than a refusal — correct, and worth
// pinning so that a later "fix" does not turn it into a 403.
func TestNotificationsAndSearchAnswerEveryAuthenticatedCaller(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	for _, path := range []string{"/api/v1/notifications", "/api/v1/search?q=anything"} {
		recorder := call(t, handler, http.MethodGet, path, "unmapped")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s answered %d for an unmapped principal, want 200", path, recorder.Code)
		}
		var payload struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
		if len(payload.Data) != 0 {
			t.Errorf("GET %s returned %d items to a principal with no access", path, len(payload.Data))
		}
	}
}

// A human token must not reach the machine API, and an administrator is no
// exception: the surfaces are separated by identity kind, not by privilege.
func TestNoHumanTokenReachesTheMachineAPI(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	for _, r := range routes() {
		if r.Auth != authService {
			continue
		}
		path := strings.ReplaceAll(r.Path, "{id}", "DOC-000001")
		recorder := call(t, handler, r.Method, path, "administrator")
		if recorder.Code == http.StatusOK {
			t.Errorf("an administrator's human token reached %s %s", r.Method, r.Path)
		}
	}
}

// A resource the caller may not see answers 404, never 403: the two are
// deliberately indistinguishable so that probing ids cannot enumerate what
// exists. Here the resource genuinely does not exist either, which is the
// point — both cases must look the same.
func TestAnAddressedResourceTheCallerMayNotSeeAnswersNotFound(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	// An id nothing maps to, read by a caller who holds the permission: the
	// answer must be 404, and never 403, because a 403 here would confirm the
	// id is real and merely out of reach.
	unknown := call(t, handler, http.MethodGet, "/api/v1/clients/CL-999999", "administrator")
	if unknown.Code != http.StatusNotFound {
		t.Errorf("an unmapped id answered %d, want 404", unknown.Code)
	}

	// A caller without the permission is refused before any lookup, so the id
	// cannot influence the answer at all.
	first := call(t, handler, http.MethodGet, "/api/v1/clients/CL-000001", "qa")
	second := call(t, handler, http.MethodGet, "/api/v1/clients/CL-999999", "qa")
	if first.Code != second.Code {
		t.Errorf("a refused caller learned something from the id: %d for a real one, %d for an invented one",
			first.Code, second.Code)
	}
}

// Audited actions carry a correlation id. Not every request is audited — the
// platform records mutations and identity-significant reads, not collection
// reads — so this asserts what it actually does rather than what a first
// reading of "every result is traceable" suggests.
func TestAuditedActionsCarryTheirCorrelationID(t *testing.T) {
	application, handler := integrationApp(t, matrixPrincipals())

	// Allocating a Global ID is audited, and an administrator may do it.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/global-ids",
		strings.NewReader(`{"entity_type":"client","source":"espocrm","source_id":"acc-audited"}`))
	request.Header.Set("Authorization", "Bearer administrator")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/global-ids answered %d: %s", recorder.Code, recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events, err := application.db.ListAudit(ctx, 50)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("an audited action left no trail")
	}
	for _, event := range events {
		if event.RequestID == "" {
			t.Errorf("audit event %q carries no request id; it cannot be correlated with anything", event.Action)
		}
		if event.GlobalUserID == "" {
			t.Errorf("audit event %q names no actor", event.Action)
		}
	}
}

// An unconfigured adapter answers 503 — for collections as well as for detail
// reads — and never 404 or an empty list.
//
// That is the right behaviour and worth pinning, because the tempting
// alternative is worse: an empty collection would tell an operator the platform
// knows of no clients, when what is true is that nobody has told it where to
// look. A missing integration must not be indistinguishable from missing data.
//
// This test corrected two documents of mine that claimed the opposite.
func TestAnUnconfiguredAdapterIsUnavailableRatherThanEmpty(t *testing.T) {
	application, handler := integrationApp(t, matrixPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entity, err := application.db.CreateGlobalEntity(ctx, "client", "espocrm", "acc-1", "", nil)
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}

	collection := call(t, handler, http.MethodGet, "/api/v1/clients", "administrator")
	if collection.Code == http.StatusOK {
		t.Fatal("an unconfigured adapter must not answer an empty collection; that reads as 'no clients exist'")
	}
	if collection.Code != http.StatusServiceUnavailable {
		t.Fatalf("the collection answered %d, want 503", collection.Code)
	}

	detail := call(t, handler, http.MethodGet, "/api/v1/clients/"+entity.GlobalID, "administrator")
	if detail.Code == http.StatusNotFound {
		t.Fatal("a known mapping must not be reported as not found because its source system is unconfigured")
	}
	if detail.Code != http.StatusServiceUnavailable {
		t.Fatalf("a known mapping with no adapter answered %d, want 503", detail.Code)
	}
	if body := detail.Body.String(); strings.Contains(strings.ToLower(body), "password") ||
		strings.Contains(body, "127.0.0.1") {
		t.Errorf("the failure discloses internal detail: %s", body)
	}

	// Both answers must be machine-readable. The error contract tells a caller
	// to branch on code rather than on the human-readable summary, and a
	// deployment that deliberately leaves an integration out is the condition a
	// caller is most likely to meet. Without a code the only honest thing a
	// client can render is "something went wrong", which reports a supported
	// configuration as a fault.
	for name, recorder := range map[string]*httptest.ResponseRecorder{
		"collection": collection,
		"detail":     detail,
	} {
		var failure struct {
			Error  string `json:"error"`
			Code   string `json:"code"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
			t.Fatalf("%s: the failure is not the platform's error shape: %v", name, err)
		}
		if failure.Code != "adapter_not_configured" {
			t.Errorf("%s: code is %q, want adapter_not_configured", name, failure.Code)
		}
		if failure.Source != "espocrm" {
			t.Errorf("%s: source is %q, want espocrm — a caller must be able to say which integration is missing", name, failure.Source)
		}
		if failure.Error == "" {
			t.Errorf("%s: the failure carries no human-readable summary", name)
		}
	}
}

// An identity is created once and keeps its Global ID. Two requests from the
// same person must not become two identities, which would split their audit
// trail and their scope grants in half.
func TestRepeatedSignInKeepsOneIdentity(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	var ids []string
	for i := 0; i < 3; i++ {
		recorder := call(t, handler, http.MethodGet, "/api/v1/me", "developer")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/me answered %d", recorder.Code)
		}
		var me struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &me); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids = append(ids, me.ID)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("the same person received two Global IDs: %v", ids)
		}
		if !strings.HasPrefix(id, "USR-") {
			t.Fatalf("unexpected Global user ID: %q", id)
		}
	}

	// Three identical Global IDs is the assertion. Counting rows would need a
	// store method that exists only for this test, and a production method
	// added to satisfy a test is a worse trade than the assertion it buys.
}

// An invalid token is refused, and the refusal says nothing about why.
func TestAnInvalidTokenIsRefusedWithoutExplanation(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	recorder := call(t, handler, http.MethodGet, "/api/v1/me", "not-a-real-token")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown token answered %d, want 401", recorder.Code)
	}
	body := strings.ToLower(recorder.Body.String())
	for _, leak := range []string{"not-a-real-token", "userinfo", "http://127.0.0.1"} {
		if strings.Contains(body, leak) {
			t.Errorf("the refusal discloses %q: %s", leak, recorder.Body.String())
		}
	}
}

// A failed audit write is counted, not only logged.
//
// The platform already makes this argument twice in its own source, for
// published events and for raised notifications: an outcome that is only
// logged is invisible on a dashboard, and a pipeline that has silently stopped
// looks exactly like one with nothing to do. It was not applied to the audit
// write, which is the record least able to survive being missed.
//
// The asymmetry is the point. An event that fails to publish can be re-derived
// from the state that produced it; a notification can be raised again. An
// audit record that was never written cannot be reconstructed from anything,
// because its whole purpose is to record that somebody did something to a
// system that keeps no other trace of who asked. A scope grant that succeeded
// with no audit row is a live permission change nobody can attribute.
//
// The request is deliberately still not failed when the audit write fails: the
// action it describes has already happened, and reporting failure for a
// completed action would be a lie in the other direction. What changes is that
// the hole is now countable.
func TestAFailedAuditWriteIsCountedRatherThanOnlyLogged(t *testing.T) {
	application, _ := integrationApp(t, matrixPrincipals())

	const action = "audit.counter.probe"
	access := meResponse{Subject: "probe-subject", ID: "USR-probe"}

	before := auditOutcome(t, action, "written")

	request := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
	application.audit(request, access, action, "probe", "probe-1", nil)

	if got := auditOutcome(t, action, "written"); got != before+1 {
		t.Errorf("written count = %v, want %v; a successful audit write must be counted", got, before+1)
	}

	// A context already past its deadline is how the database looks to this
	// code path when it is unreachable: InsertAudit returns an error and the
	// row does not exist.
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	failing := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil).WithContext(dead)

	failedBefore := auditOutcome(t, action, "failed")
	application.audit(failing, access, action, "probe", "probe-2", nil)
	if got := auditOutcome(t, action, "failed"); got != failedBefore+1 {
		t.Errorf("failed count = %v, want %v; an audit write that did not happen must not be silent", got, failedBefore+1)
	}

	// And it really did not happen: the counter is reporting a genuine loss
	// rather than a mislabelled success.
	ctx, listCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer listCancel()
	events, err := application.db.ListAudit(ctx, 200)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	found := map[string]bool{}
	for _, event := range events {
		if event.Action == action {
			found[event.ResourceID] = true
		}
	}
	if !found["probe-1"] {
		t.Error("the write counted as written left no row")
	}
	if found["probe-2"] {
		t.Error("the write counted as failed left a row; the counter is describing something other than what happened")
	}
}

// auditOutcome reads one sample straight out of the rendered exposition, so
// the test reads what an operator would scrape rather than an internal
// counter's field.
func auditOutcome(t *testing.T, action, outcome string) float64 {
	t.Helper()

	var rendered strings.Builder
	metricsRegistry.Render(&rendered)

	want := `bsystem_audit_writes_total{action="` + action + `",outcome="` + outcome + `"}`
	for _, line := range strings.Split(rendered.String(), "\n") {
		if !strings.HasPrefix(line, want) {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("cannot parse %q: %v", line, err)
		}
		return value
	}
	// A counter publishes no series until something increments it, so absence
	// is zero rather than a failure.
	return 0
}

// No database internal reaches a caller, and an unsupported entity type is
// told apart from a platform failure.
//
// The allocation handler answered every CreateGlobalEntity failure as HTTP 400
// with err.Error() in the body. For the one failure a caller causes that was
// right; for a database error it returned a raw SQL message — naming the
// table, the column tuple and the constraint — as a client mistake, on the
// status nobody retries. docs/ARCHITECTURE.md and the platform's error model
// both say upstream and internal errors must not leak internal topology, and
// nothing checked this path.
func TestGlobalIDAllocationLeaksNoDatabaseInternals(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/global-ids", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer administrator")
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	// The caller's own mistake: a type the platform mints no prefix for.
	// Answered 400, because it is the caller's, and carrying a code because
	// the contract tells callers to branch on one.
	refused := post(`{"entity_type":"not-an-entity","source":"espocrm","source_id":"x-1"}`)
	if refused.Code != http.StatusBadRequest {
		t.Errorf("unsupported entity type answered %d, want 400: %s", refused.Code, refused.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(refused.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "unsupported_entity_type" {
		t.Errorf("code = %q, want unsupported_entity_type; a caller told to branch on code needs one", body["code"])
	}

	// Whatever the platform answers here, it must not be a database's idea of
	// an error message. These are the fragments that would give away the
	// schema.
	for _, leak := range []string{"SQLSTATE", "constraint", "global_entities", "pgx", "duplicate key"} {
		if strings.Contains(refused.Body.String(), leak) {
			t.Errorf("the response carries %q: %s", leak, refused.Body.String())
		}
	}

	// A duplicate allocation is no longer a failure at all: the second caller
	// is given the id the first one got.
	first := post(`{"entity_type":"client","source":"espocrm","source_id":"acc-twice"}`)
	second := post(`{"entity_type":"client","source":"espocrm","source_id":"acc-twice"}`)
	if first.Code != http.StatusOK && first.Code != http.StatusCreated {
		t.Fatalf("first allocation answered %d: %s", first.Code, first.Body.String())
	}
	if second.Code != first.Code {
		t.Errorf("second allocation answered %d, first answered %d: %s", second.Code, first.Code, second.Body.String())
	}
	var firstEntity, secondEntity map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &firstEntity)
	_ = json.Unmarshal(second.Body.Bytes(), &secondEntity)
	if firstEntity["global_id"] != secondEntity["global_id"] {
		t.Errorf("the same upstream record was answered %v then %v", firstEntity["global_id"], secondEntity["global_id"])
	}
}
