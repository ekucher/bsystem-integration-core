package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
)

// ---------- pure unit tests: no database, no HTTP ----------

func TestRolesFromWireAcceptsTheClosedHumanRoleVocabulary(t *testing.T) {
	t.Parallel()
	ids, ok := rolesFromWire([]string{"admin", "Manager", " qa "})
	if !ok {
		t.Fatal("rolesFromWire() ok = false, want true")
	}
	want := []string{"administrator", "manager", "qa"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("rolesFromWire() = %v, want %v", ids, want)
	}
}

func TestRolesFromWireRejectsAnythingOutsideTheVocabulary(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"administrator", "service-core", "Root", "", "  "} {
		if _, ok := rolesFromWire([]string{bad}); ok {
			t.Errorf("rolesFromWire([%q]) ok = true, want false — %q is not a wire-level HumanRole", bad, bad)
		}
	}
}

func TestRolesFromWireAcceptsAnEmptySlice(t *testing.T) {
	t.Parallel()
	ids, ok := rolesFromWire(nil)
	if !ok || len(ids) != 0 {
		t.Errorf("rolesFromWire(nil) = (%v, %v), want ([], true)", ids, ok)
	}
}

func TestRolesToWireTranslatesInternalIDsAndDropsUnrepresentable(t *testing.T) {
	t.Parallel()
	got := rolesToWire([]string{"administrator", "qa", "service-core", "developer"})
	want := []string{"admin", "qa", "developer"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rolesToWire() = %v, want %v", got, want)
	}
}

func TestModuleHumanRoleBridgeRoundTrips(t *testing.T) {
	t.Parallel()
	wire := []string{"admin", "manager", "developer", "qa", "support", "devops", "customer"}
	ids, ok := rolesFromWire(wire)
	if !ok {
		t.Fatal("rolesFromWire() ok = false")
	}
	back := rolesToWire(ids)
	sort.Strings(wire)
	sort.Strings(back)
	if !reflect.DeepEqual(wire, back) {
		t.Errorf("round-trip = %v, want %v", back, wire)
	}
}

var testOrigins = []string{"https://redmine.bsystem.example", "https://wiki.bsystem.example"}

func TestValidateLaunchURLAcceptsACanonicalOriginAndPath(t *testing.T) {
	t.Parallel()
	got, err := validateLaunchURL("https://redmine.bsystem.example/issues/1", testOrigins)
	if err != nil {
		t.Fatalf("validateLaunchURL() error = %v", err)
	}
	if got != "https://redmine.bsystem.example/issues/1" {
		t.Errorf("validateLaunchURL() = %q", got)
	}
}

func TestValidateLaunchURLAcceptsABareCanonicalOrigin(t *testing.T) {
	t.Parallel()
	if _, err := validateLaunchURL("https://redmine.bsystem.example", testOrigins); err != nil {
		t.Errorf("validateLaunchURL() error = %v", err)
	}
}

func TestValidateLaunchURLAcceptsEmptyAsNoLaunchURLYet(t *testing.T) {
	t.Parallel()
	got, err := validateLaunchURL("  ", testOrigins)
	if err != nil || got != "" {
		t.Errorf("validateLaunchURL(empty) = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestValidateLaunchURLRejectsHTTP(t *testing.T) {
	t.Parallel()
	if _, err := validateLaunchURL("http://redmine.bsystem.example/issues/1", testOrigins); err != errLaunchURLProtocol {
		t.Errorf("validateLaunchURL(http) error = %v, want errLaunchURLProtocol", err)
	}
}

func TestValidateLaunchURLRejectsUnsafeProtocols(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"javascript:alert(1)", "data:text/html,<script>alert(1)</script>", "file:///etc/passwd"} {
		if _, err := validateLaunchURL(raw, testOrigins); err != errLaunchURLProtocol {
			t.Errorf("validateLaunchURL(%q) error = %v, want errLaunchURLProtocol", raw, err)
		}
	}
}

func TestValidateLaunchURLRejectsAMalformedValue(t *testing.T) {
	t.Parallel()
	if _, err := validateLaunchURL("not a url", testOrigins); err != errLaunchURLProtocol {
		t.Errorf("validateLaunchURL(malformed) error = %v, want errLaunchURLProtocol", err)
	}
}

func TestValidateLaunchURLRejectsANonCanonicalOrigin(t *testing.T) {
	t.Parallel()
	if _, err := validateLaunchURL("https://evil.example.com/phish", testOrigins); err != errLaunchURLOrigin {
		t.Errorf("validateLaunchURL(disallowed origin) error = %v, want errLaunchURLOrigin", err)
	}
}

// A path segment cannot smuggle a different origin past the allowlist check:
// url.Parse resolves the authority once, from the string up to the first
// unescaped "/", so anything after "redmine.bsystem.example/" is path, not a
// second host — this pins that Go's own parser, not a hand-rolled check,
// carries that guarantee for the exact shape this handler relies on.
func TestValidateLaunchURLDoesNotLetAPathSegmentActAsAnOrigin(t *testing.T) {
	t.Parallel()
	got, err := validateLaunchURL("https://redmine.bsystem.example/@evil.example.com", testOrigins)
	if err != nil {
		t.Fatalf("validateLaunchURL() error = %v", err)
	}
	if got != "https://redmine.bsystem.example/@evil.example.com" {
		t.Errorf("validateLaunchURL() = %q, want the path preserved under the real origin", got)
	}
}

func TestModuleAllowedOriginsIsEmptyWhenUnconfigured(t *testing.T) {
	t.Setenv("MODULE_ALLOWED_ORIGINS", "")
	if got := moduleAllowedOrigins(); len(got) != 0 {
		t.Errorf("moduleAllowedOrigins() = %v, want empty", got)
	}
}

func TestModuleAllowedOriginsTrimsAndDropsEmptyEntries(t *testing.T) {
	t.Setenv("MODULE_ALLOWED_ORIGINS", " https://a.bsystem.example/ , ,https://b.bsystem.example")
	got := moduleAllowedOrigins()
	want := []string{"https://a.bsystem.example", "https://b.bsystem.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("moduleAllowedOrigins() = %v, want %v", got, want)
	}
}

// ---------- integration tests: real routes, real RBAC, real database ----------

func patchJSON(t *testing.T, handler http.Handler, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// Only the Administrator role holds module.admin (migration 018 grants it to
// "administrator" alone — no sub-admin by default, matching the HUB spec's
// "single global permission"). Every other seeded role, including Manager,
// must be refused.
func TestModuleAdministrationPermissionMatrix(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	for _, probe := range []struct {
		actor, method, path string
	}{
		{"manager", http.MethodGet, "/api/v1/admin/modules"},
		{"developer", http.MethodGet, "/api/v1/admin/modules"},
		{"qa", http.MethodGet, "/api/v1/admin/modules"},
		{"devops", http.MethodGet, "/api/v1/admin/modules"},
		{"customer", http.MethodGet, "/api/v1/admin/modules"},
		{"unmapped", http.MethodGet, "/api/v1/admin/modules"},
		{"manager", http.MethodGet, "/api/v1/admin/modules/allowed-origins"},
		{"customer", http.MethodPost, "/api/v1/admin/modules"},
		{"manager", http.MethodPatch, "/api/v1/admin/modules/redmine"},
	} {
		recorder := call(t, handler, probe.method, probe.path, probe.actor)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s %s answered %d, want 403 (body %s)", probe.actor, probe.method, probe.path, recorder.Code, recorder.Body.String())
		}
	}

	if recorder := call(t, handler, http.MethodGet, "/api/v1/admin/modules", "administrator"); recorder.Code != http.StatusOK {
		t.Errorf("administrator GET /api/v1/admin/modules answered %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if recorder := call(t, handler, http.MethodGet, "/api/v1/admin/modules/allowed-origins", "administrator"); recorder.Code != http.StatusOK {
		t.Errorf("administrator GET .../allowed-origins answered %d, want 200", recorder.Code)
	}
}

// The complete spec flow, exercised against the real store: create (disabled,
// no roles) -> assign roles -> configure launch data -> activate -> the
// launcher's own RBAC-filtered endpoint reflects it for a granted role and
// omits it for one that was never granted.
func TestModuleAdministrationFullLifecycle(t *testing.T) {
	t.Setenv("MODULE_ALLOWED_ORIGINS", "https://kb.bsystem.example")
	_, handler := integrationApp(t, matrixPrincipals())

	created := post(t, handler, "/api/v1/admin/modules", "administrator", map[string]any{
		"id": "kb", "name": "Knowledge Base", "description": "Runbooks", "status": "disabled",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: answered %d (body %s)", created.Code, created.Body.String())
	}
	var createdBody struct {
		Status       string   `json:"status"`
		AllowedRoles []string `json:"allowed_roles"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if createdBody.Status != "disabled" || len(createdBody.AllowedRoles) != 0 {
		t.Fatalf("created module = %+v, want status=disabled, allowed_roles=[]", createdBody)
	}

	// Activating before any role is assigned must be refused.
	prematureActivate := patchJSON(t, handler, "/api/v1/admin/modules/kb", "administrator", map[string]any{"status": "active"})
	if prematureActivate.Code != http.StatusUnprocessableEntity {
		t.Fatalf("premature activate: answered %d, want 422 (body %s)", prematureActivate.Code, prematureActivate.Body.String())
	}

	// Assign roles.
	rolesResp := patchJSON(t, handler, "/api/v1/admin/modules/kb", "administrator", map[string]any{"allowed_roles": []string{"developer"}})
	if rolesResp.Code != http.StatusOK {
		t.Fatalf("assign roles: answered %d (body %s)", rolesResp.Code, rolesResp.Body.String())
	}

	// Configure launch data.
	launchResp := patchJSON(t, handler, "/api/v1/admin/modules/kb", "administrator", map[string]any{"launch_url": "https://kb.bsystem.example/runbooks"})
	if launchResp.Code != http.StatusOK {
		t.Fatalf("configure launch_url: answered %d (body %s)", launchResp.Code, launchResp.Body.String())
	}

	// Activate — now allowed, since roles were assigned above.
	activateResp := patchJSON(t, handler, "/api/v1/admin/modules/kb", "administrator", map[string]any{"status": "active"})
	if activateResp.Code != http.StatusOK {
		t.Fatalf("activate: answered %d (body %s)", activateResp.Code, activateResp.Body.String())
	}
	var final struct {
		Status       string   `json:"status"`
		LaunchURL    string   `json:"launch_url"`
		AllowedRoles []string `json:"allowed_roles"`
		UpdatedBy    string   `json:"updated_by"`
	}
	if err := json.Unmarshal(activateResp.Body.Bytes(), &final); err != nil {
		t.Fatalf("decode activate response: %v", err)
	}
	if final.Status != "active" || final.LaunchURL != "https://kb.bsystem.example/runbooks" || !reflect.DeepEqual(final.AllowedRoles, []string{"developer"}) {
		t.Fatalf("final module = %+v, want active/kb runbooks url/[developer]", final)
	}
	if final.UpdatedBy == "" {
		t.Error("updated_by is empty; the acting principal should be recorded")
	}

	// The real launcher endpoint, RBAC-filtered by Core — not asserted via
	// component-local state, the actual /api/v1/modules a launcher client
	// would call.
	developerView := call(t, handler, http.MethodGet, "/api/v1/modules", "developer")
	if developerView.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/modules as developer: answered %d", developerView.Code)
	}
	var developerModules []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(developerView.Body.Bytes(), &developerModules); err != nil {
		t.Fatalf("decode launcher response: %v", err)
	}
	if !containsID(developerModules, "kb") {
		t.Errorf("developer's launcher list = %v, want it to contain the newly activated \"kb\" module", developerModules)
	}

	customerView := call(t, handler, http.MethodGet, "/api/v1/modules", "customer")
	if customerView.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/modules as customer: answered %d", customerView.Code)
	}
	var customerModules []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(customerView.Body.Bytes(), &customerModules); err != nil {
		t.Fatalf("decode launcher response: %v", err)
	}
	if containsID(customerModules, "kb") {
		t.Errorf("customer's launcher list = %v, want it to NOT contain \"kb\" — customer was never granted it", customerModules)
	}
}

func containsID(items []struct {
	ID string `json:"id"`
}, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func TestModuleAdministrationRejectsADuplicateID(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	body := map[string]any{"id": "dup-module", "name": "First", "description": "d"}
	first := post(t, handler, "/api/v1/admin/modules", "administrator", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: answered %d (body %s)", first.Code, first.Body.String())
	}
	second := post(t, handler, "/api/v1/admin/modules", "administrator", body)
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate create: answered %d, want 409 (body %s)", second.Code, second.Body.String())
	}
}

func TestModuleAdministrationRejectsAnUnknownRole(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	recorder := post(t, handler, "/api/v1/admin/modules", "administrator", map[string]any{
		"id": "bad-role-module", "name": "N", "description": "d", "allowed_roles": []string{"superadmin"},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("create with unknown role: answered %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestModuleAdministrationUpdateOfAnUnknownModuleAnswersNotFound(t *testing.T) {
	_, handler := integrationApp(t, matrixPrincipals())

	recorder := patchJSON(t, handler, "/api/v1/admin/modules/does-not-exist", "administrator", map[string]any{"status": "maintenance"})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("patch unknown module: answered %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestModuleAdministrationRejectsALaunchURLOffTheAllowlist(t *testing.T) {
	t.Setenv("MODULE_ALLOWED_ORIGINS", "https://kb.bsystem.example")
	_, handler := integrationApp(t, matrixPrincipals())

	recorder := post(t, handler, "/api/v1/admin/modules", "administrator", map[string]any{
		"id": "phish-module", "name": "N", "description": "d", "launch_url": "https://evil.example.com/phish",
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("create with disallowed origin: answered %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// An omitted PATCH field must not be interpreted as "clear this value" — a
// second PATCH touching only one field must not blank out what an earlier
// PATCH set on a different field.
func TestModuleAdministrationPartialPatchLeavesOtherFieldsAlone(t *testing.T) {
	t.Setenv("MODULE_ALLOWED_ORIGINS", "https://kb.bsystem.example")
	_, handler := integrationApp(t, matrixPrincipals())

	create := post(t, handler, "/api/v1/admin/modules", "administrator", map[string]any{
		"id": "partial-module", "name": "Original Name", "description": "Original description",
		"launch_url": "https://kb.bsystem.example/x",
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("create: answered %d (body %s)", create.Code, create.Body.String())
	}

	// Touch only description.
	patched := patchJSON(t, handler, "/api/v1/admin/modules/partial-module", "administrator", map[string]any{"description": "Updated description"})
	if patched.Code != http.StatusOK {
		t.Fatalf("patch description: answered %d (body %s)", patched.Code, patched.Body.String())
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		LaunchURL   string `json:"launch_url"`
	}
	if err := json.Unmarshal(patched.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Name != "Original Name" || body.LaunchURL != "https://kb.bsystem.example/x" {
		t.Errorf("after patching only description, module = %+v; name/launch_url should be unchanged", body)
	}
	if body.Description != "Updated description" {
		t.Errorf("description = %q, want it to have actually changed", body.Description)
	}
}
