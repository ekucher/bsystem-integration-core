package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// relationshipRowCount reports how many entity_relationships rows connect the
// two Global IDs, in either direction. Every negative test below uses this
// rather than trusting an HTTP status alone: a refusal that quietly wrote the
// row anyway would be worse than no refusal at all.
func relationshipRowCount(t *testing.T, application *app, fromGlobalID, toGlobalID string) int {
	t.Helper()
	pool := auditPool(t, application)
	var count int
	err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM entity_relationships
WHERE (from_global_id=$1 AND to_global_id=$2) OR (from_global_id=$2 AND to_global_id=$1)`,
		fromGlobalID, toGlobalID).Scan(&count)
	if err != nil {
		t.Fatalf("count relationships between %s and %s: %v", fromGlobalID, toGlobalID, err)
	}
	return count
}

// lastAuditMetadata returns the metadata of the most recent audit_events row
// for the given action and resource id, decoded from JSONB. It fails the test
// outright if no such row exists, because the whole point of calling it is to
// prove one does.
func lastAuditMetadata(t *testing.T, application *app, action, resourceID string) map[string]any {
	t.Helper()
	pool := auditPool(t, application)
	var globalUserID string
	var raw []byte
	err := pool.QueryRow(context.Background(), `
SELECT global_user_id, metadata FROM audit_events
WHERE action=$1 AND resource_type='relationship' AND resource_id=$2
ORDER BY occurred_at DESC LIMIT 1`, action, resourceID).Scan(&globalUserID, &raw)
	if err != nil {
		t.Fatalf("no audit_events row for action=%s resource_id=%s: %v", action, resourceID, err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decode audit metadata: %v", err)
	}
	metadata["_global_user_id"] = globalUserID
	return metadata
}

// relationshipPrincipals extends the isolation matrix with a service identity
// and reuses "developer" as the end user a service acts on behalf of.
func relationshipPrincipals() map[string]map[string]any {
	principals := matrixPrincipals()
	principals["reporter-service"] = principal("reporter-service-1", "BSYSTEM-Services")
	return principals
}

// requestWithHeaders issues a request carrying both the service's own
// Authorization bearer and an arbitrary set of extra headers — the on-behalf-of
// header among them — so a test can exercise the dual-actor requirement
// exactly as a real plugin would send it.
func requestWithHeaders(t *testing.T, handler http.Handler, method, path, token string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	request := httptest.NewRequest(method, path, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// A relationship mutation is refused before it touches the store when the
// on-behalf-of header is absent, even though the service's own token is
// perfectly valid — and, critically, no row is written for it anyway.
func TestRelationshipCreationRequiresAnOnBehalfOfToken(t *testing.T) {
	application, handler := integrationApp(t, relationshipPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	from, err := application.db.CreateGlobalEntity(ctx, "test_case", "qa", "tc-no-actor", "", nil)
	if err != nil {
		t.Fatalf("seed from entity: %v", err)
	}
	to, err := application.db.CreateGlobalEntity(ctx, "requirement", "qa", "req-no-actor", "", nil)
	if err != nil {
		t.Fatalf("seed to entity: %v", err)
	}

	recorder := requestWithHeaders(t, handler, http.MethodPost, "/api/service/v1/relationships", "reporter-service", nil,
		`{"from_global_id":"`+from.GlobalID+`","relation_type":"tests","to_global_id":"`+to.GlobalID+`"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d without an on-behalf-of token, want 401: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "on_behalf_of_required" {
		t.Errorf("code = %q, want on_behalf_of_required", body["code"])
	}
	if count := relationshipRowCount(t, application, from.GlobalID, to.GlobalID); count != 0 {
		t.Errorf("a mutation refused for missing on-behalf-of still wrote %d row(s)", count)
	}
}

// An on-behalf-of header is not trusted at face value: a token that does not
// verify is refused exactly like an invalid Authorization token would be,
// even though the calling service's own credential is fine. This is the
// critical negative security test: a caller who merely claims to be acting
// for a user, without a token that verifies as that user, cannot attribute a
// relationship to them.
func TestRelationshipCreationRejectsAnUnverifiableOnBehalfOfToken(t *testing.T) {
	application, handler := integrationApp(t, relationshipPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	from, err := application.db.CreateGlobalEntity(ctx, "test_case", "qa", "tc-spoofed", "", nil)
	if err != nil {
		t.Fatalf("seed from entity: %v", err)
	}
	to, err := application.db.CreateGlobalEntity(ctx, "requirement", "qa", "req-spoofed", "", nil)
	if err != nil {
		t.Fatalf("seed to entity: %v", err)
	}

	recorder := requestWithHeaders(t, handler, http.MethodPost, "/api/service/v1/relationships", "reporter-service",
		map[string]string{"X-On-Behalf-Of": "Bearer not-a-real-user-token"},
		`{"from_global_id":"`+from.GlobalID+`","relation_type":"tests","to_global_id":"`+to.GlobalID+`"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d for an unverifiable on-behalf-of token, want 401: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "on_behalf_of_invalid" {
		t.Errorf("code = %q, want on_behalf_of_invalid", body["code"])
	}
	if count := relationshipRowCount(t, application, from.GlobalID, to.GlobalID); count != 0 {
		t.Errorf("a mutation with a forged on-behalf-of identity still wrote %d row(s)", count)
	}
}

// The full lifecycle: a service creates a relationship on a verified human's
// behalf, both the human and machine read paths see it from either endpoint,
// and deleting it removes only the edge — never either endpoint's own record —
// and is safe to replay.
func TestServiceRelationshipLifecycleOnBehalfOfAVerifiedUser(t *testing.T) {
	application, handler := integrationApp(t, relationshipPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	testCase, err := application.db.CreateGlobalEntity(ctx, "test_case", "qa", "tc-lifecycle", "", nil)
	if err != nil {
		t.Fatalf("seed test case: %v", err)
	}
	requirement, err := application.db.CreateGlobalEntity(ctx, "requirement", "qa", "req-lifecycle", "", nil)
	if err != nil {
		t.Fatalf("seed requirement: %v", err)
	}

	onBehalfOf := map[string]string{"X-On-Behalf-Of": "Bearer developer"}
	createBody := `{"from_global_id":"` + testCase.GlobalID + `","relation_type":"tests","to_global_id":"` + requirement.GlobalID + `"}`

	created := requestWithHeaders(t, handler, http.MethodPost, "/api/service/v1/relationships", "reporter-service", onBehalfOf, createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create answered %d, want 201: %s", created.Code, created.Body.String())
	}
	var relationship struct {
		ID                 int64  `json:"id"`
		CreatedByUserID    string `json:"created_by_user_id"`
		CreatedByServiceID string `json:"created_by_service_id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &relationship); err != nil {
		t.Fatalf("decode created relationship: %v", err)
	}
	if !strings.HasPrefix(relationship.CreatedByUserID, "USR-") {
		t.Errorf("created_by_user_id = %q, want the verified developer's own USR-*, not the raw header", relationship.CreatedByUserID)
	}
	if !strings.HasPrefix(relationship.CreatedByServiceID, "SVC-") {
		t.Errorf("created_by_service_id = %q, want the calling service's own SVC-*", relationship.CreatedByServiceID)
	}

	// The audit trail records both actors, not just the service's own
	// identity: an edge attributed to nobody in particular would defeat the
	// entire point of requiring an on-behalf-of token.
	resourceID := strconv.FormatInt(relationship.ID, 10)
	audited := lastAuditMetadata(t, application, "relationship.created", resourceID)
	if audited["_global_user_id"] != relationship.CreatedByUserID {
		t.Errorf("audit_events.global_user_id = %q, want %q", audited["_global_user_id"], relationship.CreatedByUserID)
	}
	if audited["created_by_user_id"] != relationship.CreatedByUserID {
		t.Errorf("audit metadata created_by_user_id = %v, want %q", audited["created_by_user_id"], relationship.CreatedByUserID)
	}
	if audited["created_by_service_id"] != relationship.CreatedByServiceID {
		t.Errorf("audit metadata created_by_service_id = %v, want %q", audited["created_by_service_id"], relationship.CreatedByServiceID)
	}
	if audited["from_global_id"] != testCase.GlobalID || audited["to_global_id"] != requirement.GlobalID {
		t.Errorf("audit metadata endpoints = %v/%v, want %s/%s", audited["from_global_id"], audited["to_global_id"], testCase.GlobalID, requirement.GlobalID)
	}

	// Idempotent replay: the identical request again is 200, not a second row.
	replay := requestWithHeaders(t, handler, http.MethodPost, "/api/service/v1/relationships", "reporter-service", onBehalfOf, createBody)
	if replay.Code != http.StatusOK {
		t.Fatalf("replayed create answered %d, want 200: %s", replay.Code, replay.Body.String())
	}
	var replayed struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(replay.Body.Bytes(), &replayed)
	if replayed.ID != relationship.ID {
		t.Fatalf("replay resolved to a different row: %d != %d", replayed.ID, relationship.ID)
	}

	// The human read path sees it, labelled correctly from each side.
	fromTest := call(t, handler, http.MethodGet, "/api/v1/global-ids/"+testCase.GlobalID+"/relationships", "developer")
	if fromTest.Code != http.StatusOK {
		t.Fatalf("human list answered %d: %s", fromTest.Code, fromTest.Body.String())
	}
	if !strings.Contains(fromTest.Body.String(), `"relation_type":"tests"`) || !strings.Contains(fromTest.Body.String(), `"direction":"outgoing"`) {
		t.Errorf("human list from the test case did not show an outgoing 'tests' edge: %s", fromTest.Body.String())
	}

	fromRequirement := call(t, handler, http.MethodGet, "/api/v1/global-ids/"+requirement.GlobalID+"/relationships", "developer")
	if fromRequirement.Code != http.StatusOK {
		t.Fatalf("human list answered %d: %s", fromRequirement.Code, fromRequirement.Body.String())
	}
	if !strings.Contains(fromRequirement.Body.String(), `"relation_type":"tested-by"`) || !strings.Contains(fromRequirement.Body.String(), `"direction":"incoming"`) {
		t.Errorf("human list from the requirement did not show an incoming 'tested-by' edge: %s", fromRequirement.Body.String())
	}

	// The machine read path answers identically, without an on-behalf-of token.
	serviceList := call(t, handler, http.MethodGet, "/api/service/v1/relationships?global_id="+testCase.GlobalID, "reporter-service")
	if serviceList.Code != http.StatusOK {
		t.Fatalf("service list answered %d: %s", serviceList.Code, serviceList.Body.String())
	}
	if !strings.Contains(serviceList.Body.String(), requirement.GlobalID) {
		t.Errorf("service list did not include the requirement: %s", serviceList.Body.String())
	}

	// A human token cannot delete a relationship: the machine API stays
	// separated from the human one for this route like every other.
	humanDelete := call(t, handler, http.MethodDelete, "/api/service/v1/relationships/1", "developer")
	if humanDelete.Code == http.StatusOK || humanDelete.Code == http.StatusNoContent {
		t.Fatalf("a human token reached the relationship delete route: %d", humanDelete.Code)
	}

	deletePath := "/api/service/v1/relationships/" + strconv.FormatInt(relationship.ID, 10)
	deleted := requestWithHeaders(t, handler, http.MethodDelete, deletePath, "reporter-service", onBehalfOf, "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete answered %d, want 204: %s", deleted.Code, deleted.Body.String())
	}
	deletedAudit := lastAuditMetadata(t, application, "relationship.deleted", strconv.FormatInt(relationship.ID, 10))
	if deletedAudit["deleted_by_user_id"] != relationship.CreatedByUserID {
		t.Errorf("delete audit metadata deleted_by_user_id = %v, want %q", deletedAudit["deleted_by_user_id"], relationship.CreatedByUserID)
	}

	// Replaying the delete is still success, not a failure.
	replayedDelete := requestWithHeaders(t, handler, http.MethodDelete, deletePath, "reporter-service", onBehalfOf, "")
	if replayedDelete.Code != http.StatusNoContent {
		t.Fatalf("replayed delete answered %d, want 204: %s", replayedDelete.Code, replayedDelete.Body.String())
	}

	// The edge is gone, but neither endpoint's own record was touched.
	afterDelete := call(t, handler, http.MethodGet, "/api/v1/global-ids/"+testCase.GlobalID+"/relationships", "developer")
	if strings.Contains(afterDelete.Body.String(), requirement.GlobalID) {
		t.Errorf("the relationship survived its own deletion: %s", afterDelete.Body.String())
	}
	if _, err := application.db.ResolveGlobalEntity(ctx, testCase.GlobalID); err != nil {
		t.Errorf("deleting the relationship affected the test case's own Global ID: %v", err)
	}
	if _, err := application.db.ResolveGlobalEntity(ctx, requirement.GlobalID); err != nil {
		t.Errorf("deleting the relationship affected the requirement's own Global ID: %v", err)
	}
}

// A relationship naming a Global ID the platform does not know is refused
// rather than allocated, surfaced as the caller's mistake.
func TestRelationshipCreationRejectsAnUnknownEndpoint(t *testing.T) {
	_, handler := integrationApp(t, relationshipPrincipals())

	onBehalfOf := map[string]string{"X-On-Behalf-Of": "Bearer developer"}
	recorder := requestWithHeaders(t, handler, http.MethodPost, "/api/service/v1/relationships", "reporter-service", onBehalfOf,
		`{"from_global_id":"TST-999999999","relation_type":"tests","to_global_id":"TST-999999998"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("answered %d for unknown endpoints, want 400: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "unknown_relationship_endpoint" {
		t.Errorf("code = %q, want unknown_relationship_endpoint", body["code"])
	}
}

// A Global ID cannot relate to itself. The store's ErrSelfRelationship must
// surface as the documented normalized error, and — because this is a
// same-pair request — asserting "no row" means confirming the count between
// the pair is still zero, not merely that no *other* row exists.
func TestRelationshipCreationRejectsASelfPair(t *testing.T) {
	application, handler := integrationApp(t, relationshipPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entity, err := application.db.CreateGlobalEntity(ctx, "test_case", "qa", "tc-self", "", nil)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	onBehalfOf := map[string]string{"X-On-Behalf-Of": "Bearer developer"}
	recorder := requestWithHeaders(t, handler, http.MethodPost, "/api/service/v1/relationships", "reporter-service", onBehalfOf,
		`{"from_global_id":"`+entity.GlobalID+`","relation_type":"tests","to_global_id":"`+entity.GlobalID+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("answered %d for a self-pair, want 400: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "self_relationship" {
		t.Errorf("code = %q, want self_relationship", body["code"])
	}
	if count := relationshipRowCount(t, application, entity.GlobalID, entity.GlobalID); count != 0 {
		t.Errorf("a rejected self-pair still wrote %d row(s)", count)
	}
}

// A relation_type outside the eight-pair vocabulary (forward or inverse) is
// refused as the caller's mistake, not silently coerced or allowed through as
// free text that read paths would then have no inverse label for.
func TestRelationshipCreationRejectsAnInvalidRelationType(t *testing.T) {
	application, handler := integrationApp(t, relationshipPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	from, err := application.db.CreateGlobalEntity(ctx, "test_case", "qa", "tc-badtype", "", nil)
	if err != nil {
		t.Fatalf("seed from entity: %v", err)
	}
	to, err := application.db.CreateGlobalEntity(ctx, "requirement", "qa", "req-badtype", "", nil)
	if err != nil {
		t.Fatalf("seed to entity: %v", err)
	}

	onBehalfOf := map[string]string{"X-On-Behalf-Of": "Bearer developer"}
	recorder := requestWithHeaders(t, handler, http.MethodPost, "/api/service/v1/relationships", "reporter-service", onBehalfOf,
		`{"from_global_id":"`+from.GlobalID+`","relation_type":"owns","to_global_id":"`+to.GlobalID+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("answered %d for an invalid relation_type, want 400: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "invalid_relation_type" {
		t.Errorf("code = %q, want invalid_relation_type", body["code"])
	}
	if count := relationshipRowCount(t, application, from.GlobalID, to.GlobalID); count != 0 {
		t.Errorf("a rejected relation_type still wrote %d row(s)", count)
	}
}

// A Global ID with no relationships at all gets back an empty list, not an
// error: a zero-length result must be indistinguishable from "nothing to
// report" and never mistaken for a store failure or an access denial.
func TestListRelationshipsForAGlobalIDWithNoneReturnsAnEmptyListNotAnError(t *testing.T) {
	application, handler := integrationApp(t, relationshipPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lonely, err := application.db.CreateGlobalEntity(ctx, "test_case", "qa", "tc-lonely", "", nil)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	humanList := call(t, handler, http.MethodGet, "/api/v1/global-ids/"+lonely.GlobalID+"/relationships", "developer")
	if humanList.Code != http.StatusOK {
		t.Fatalf("human list for a relationship-free Global ID answered %d, want 200: %s", humanList.Code, humanList.Body.String())
	}
	var humanBody struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(humanList.Body.Bytes(), &humanBody); err != nil {
		t.Fatalf("decode human list: %v", err)
	}
	if humanBody.Data == nil || len(humanBody.Data) != 0 {
		t.Errorf("human list data = %#v, want an empty (non-nil) list", humanBody.Data)
	}

	serviceList := call(t, handler, http.MethodGet, "/api/service/v1/relationships?global_id="+lonely.GlobalID, "reporter-service")
	if serviceList.Code != http.StatusOK {
		t.Fatalf("service list for a relationship-free Global ID answered %d, want 200: %s", serviceList.Code, serviceList.Body.String())
	}
	var serviceBody struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(serviceList.Body.Bytes(), &serviceBody); err != nil {
		t.Fatalf("decode service list: %v", err)
	}
	if serviceBody.Data == nil || len(serviceBody.Data) != 0 {
		t.Errorf("service list data = %#v, want an empty (non-nil) list", serviceBody.Data)
	}
}

// Deleting a relationship with no Authorization at all, or with an
// on-behalf-of header missing, is refused exactly like the create path is —
// and, because delete is the one place a wrongly-authorized caller could
// destroy evidence rather than merely fail to create it, the edge must still
// be there afterward.
func TestRelationshipDeletionRequiresAuthentication(t *testing.T) {
	application, handler := integrationApp(t, relationshipPrincipals())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	from, err := application.db.CreateGlobalEntity(ctx, "test_case", "qa", "tc-delete-auth", "", nil)
	if err != nil {
		t.Fatalf("seed from entity: %v", err)
	}
	to, err := application.db.CreateGlobalEntity(ctx, "requirement", "qa", "req-delete-auth", "", nil)
	if err != nil {
		t.Fatalf("seed to entity: %v", err)
	}
	actor := platformdb.RelationshipActor{UserGlobalID: "", ServiceGlobalID: "SVC-000001"}
	relationship, _, err := application.db.CreateRelationship(ctx, from.GlobalID, "tests", to.GlobalID, actor)
	if err != nil {
		t.Fatalf("seed relationship: %v", err)
	}
	deletePath := "/api/service/v1/relationships/" + strconv.FormatInt(relationship.ID, 10)

	// No Authorization header at all.
	noAuth := call(t, handler, http.MethodDelete, deletePath, "")
	if noAuth.Code != http.StatusUnauthorized {
		t.Errorf("delete with no Authorization answered %d, want 401: %s", noAuth.Code, noAuth.Body.String())
	}

	// A valid service token but no on-behalf-of header.
	noOnBehalfOf := requestWithHeaders(t, handler, http.MethodDelete, deletePath, "reporter-service", nil, "")
	if noOnBehalfOf.Code != http.StatusUnauthorized {
		t.Errorf("delete with no on-behalf-of token answered %d, want 401: %s", noOnBehalfOf.Code, noOnBehalfOf.Body.String())
	}

	// A valid service token with an on-behalf-of header that does not verify.
	badOnBehalfOf := requestWithHeaders(t, handler, http.MethodDelete, deletePath, "reporter-service",
		map[string]string{"X-On-Behalf-Of": "Bearer not-a-real-user-token"}, "")
	if badOnBehalfOf.Code != http.StatusUnauthorized {
		t.Errorf("delete with a forged on-behalf-of token answered %d, want 401: %s", badOnBehalfOf.Code, badOnBehalfOf.Body.String())
	}

	// None of the above may have removed the edge.
	if count := relationshipRowCount(t, application, from.GlobalID, to.GlobalID); count != 1 {
		t.Errorf("relationship row count after refused deletes = %d, want 1: the edge must survive every refused delete", count)
	}
}
