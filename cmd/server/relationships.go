package main

// HTTP surface over internal/platformdb's relationship store.
//
// A relationship mutation needs two things a plain service token does not
// carry: proof of which service is asking (that is what authenticateService
// already gives every /api/service/v1/* handler) and proof of which human
// asked it to. Neither may be trusted from an unverified field, because a
// service that could assert an arbitrary USR-* on a caller-supplied header
// could attribute any edge to any user, including one who never made the
// request.
//
// The mechanism here is the narrowest extension of a primitive this platform
// already trusts: a.identify, the same verified-token resolution authHuman
// uses (cmd/server/identity.go). A caller forwards the end user's own bearer
// token in X-On-Behalf-Of, this file runs it through the identical
// verification authenticate() runs — local JWT signature verification when an
// OIDC issuer is configured, or a call to authentik's own UserInfo endpoint
// otherwise — and only a token that verification accepts yields a USR-*
// Global ID. A browser or a compromised plugin cannot spoof this by sending
// an arbitrary string: it would have to forge a signature authentik's keys
// validate, or produce a token authentik's own UserInfo endpoint accepts as
// live, either of which means they already have the user's real credential
// and are not spoofing anything. This intentionally reuses authHuman's own
// trust boundary rather than inventing a new one.
//
// This file is a thin wrapper. Every relationship semantic — self-edge
// rejection, forward/inverse dedup, actor provenance, vocabulary validation —
// lives in internal/platformdb/relationships.go and is surfaced here, never
// re-implemented.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// onBehalfOfHeader carries the end user's own bearer token, alongside the
// service's own Authorization header, in the same "Bearer <token>" shape
// authenticate() and authenticateService() already parse. It is a distinct
// header because it verifies a distinct principal: the Authorization header
// on these routes proves which service is calling, this proves on whose
// authority.
const onBehalfOfHeader = "X-On-Behalf-Of"

// onBehalfOfUser resolves and verifies the end user a service is acting for.
// It never trusts the header's value directly — it is re-verified through
// exactly the pipeline authHuman uses, so the USR-* it returns is as
// trustworthy as one authenticate() itself would have resolved. On failure it
// has already written the response and the caller must return immediately.
func (a *app) onBehalfOfUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	header := r.Header.Get(onBehalfOfHeader)
	if !strings.HasPrefix(header, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "an on-behalf-of end-user bearer token is required",
			"code":  "on_behalf_of_required",
		})
		return "", false
	}
	info, err := a.identify(r.Context(), strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		status, message := authenticationStatus(err)
		logger.WarnContext(r.Context(), "on-behalf-of authentication failed", "error", err.Error())
		writeJSON(w, status, map[string]string{
			"error": message,
			"code":  "on_behalf_of_invalid",
		})
		return "", false
	}
	username := info.PreferredUsername
	if username == "" {
		username = info.Email
	}
	globalUserID, _, err := a.db.EnsureIdentity(r.Context(), platformdb.Identity{
		Subject: info.Sub, Email: info.Email, DisplayName: info.Name, Username: username, Groups: unique(info.Groups),
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "on-behalf-of identity persistence failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "identity persistence unavailable",
			"code":  "identity_store_unavailable",
		})
		return "", false
	}
	return globalUserID, true
}

// relationshipRequest is the body of a create call.
type relationshipRequest struct {
	FromGlobalID string `json:"from_global_id"`
	RelationType string `json:"relation_type"`
	ToGlobalID   string `json:"to_global_id"`
}

// relationshipErrorResponse maps a store error onto the normalized envelope,
// following the same distinction createGlobalID draws for
// ErrUnsupportedEntityType: a sentinel from the store is the caller's mistake
// and safe to echo, and anything else is the platform's and is answered
// without its text.
func relationshipErrorResponse(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, platformdb.ErrInvalidRelationType):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_relation_type"})
	case errors.Is(err, platformdb.ErrSelfRelationship):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "self_relationship"})
	case errors.Is(err, platformdb.ErrUnknownGlobalID):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "unknown_relationship_endpoint"})
	case errors.Is(err, platformdb.ErrMissingActor):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "relationship_actor_required"})
	default:
		logger.ErrorContext(r.Context(), "relationship mutation failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":      "relationship could not be processed",
			"code":       "relationship_store_unavailable",
			"request_id": requestIDFrom(r.Context()),
		})
	}
}

// serviceCreateRelationship asserts a typed edge between two existing Global
// IDs, on behalf of a verified end user, at the request of an authenticated
// service.
func (a *app) serviceCreateRelationship(w http.ResponseWriter, r *http.Request) {
	principal := serviceFrom(r.Context())
	userGlobalID, ok := a.onBehalfOfUser(w, r)
	if !ok {
		return
	}

	var input relationshipRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	input.FromGlobalID = strings.TrimSpace(input.FromGlobalID)
	input.RelationType = strings.TrimSpace(input.RelationType)
	input.ToGlobalID = strings.TrimSpace(input.ToGlobalID)
	if input.FromGlobalID == "" || input.RelationType == "" || input.ToGlobalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "from_global_id, relation_type and to_global_id are required",
		})
		return
	}

	actor := platformdb.RelationshipActor{
		UserGlobalID:    userGlobalID,
		ServiceGlobalID: principal.ID,
		RequestID:       requestIDFrom(r.Context()),
	}
	relationship, created, err := a.db.CreateRelationship(r.Context(), input.FromGlobalID, input.RelationType, input.ToGlobalID, actor)
	if err != nil {
		relationshipErrorResponse(w, r, err)
		return
	}
	// CreateRelationship audits and queues relationship.created itself, only
	// on the path that actually wrote a row; nothing further is done here.
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, relationship)
}

// renderRelationships lists every relationship touching globalID and writes
// it as a collection, for both the human and the service read routes.
func (a *app) renderRelationships(w http.ResponseWriter, r *http.Request, globalID string) {
	globalID = strings.TrimSpace(globalID)
	if globalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "global_id is required"})
		return
	}
	views, err := a.db.ListRelationships(r.Context(), globalID)
	if err != nil {
		logger.ErrorContext(r.Context(), "relationship listing failed", "global_id", globalID, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":      "relationships could not be listed",
			"code":       "relationship_store_unavailable",
			"request_id": requestIDFrom(r.Context()),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": views})
}

// listGlobalIDRelationships is the human-facing read: every relationship
// touching one Global ID, outgoing and incoming, labelled from that Global
// ID's own point of view.
func (a *app) listGlobalIDRelationships(w http.ResponseWriter, r *http.Request) {
	a.renderRelationships(w, r, r.PathValue("id"))
}

// serviceListRelationships is the machine-facing equivalent, addressed by a
// query parameter rather than a path segment so it does not collide with the
// delete route's {id}, which names a relationship rather than a Global ID.
func (a *app) serviceListRelationships(w http.ResponseWriter, r *http.Request) {
	a.renderRelationships(w, r, r.URL.Query().Get("global_id"))
}

// serviceDeleteRelationship removes one edge by its own id. It never deletes
// or mutates either endpoint's global_entities row: only the relationship
// itself is capable of being removed here, and the store enforces that by
// only ever issuing a DELETE against entity_relationships.
func (a *app) serviceDeleteRelationship(w http.ResponseWriter, r *http.Request) {
	principal := serviceFrom(r.Context())
	userGlobalID, ok := a.onBehalfOfUser(w, r)
	if !ok {
		return
	}

	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "relationship id must be a positive integer"})
		return
	}

	actor := platformdb.RelationshipActor{
		UserGlobalID:    userGlobalID,
		ServiceGlobalID: principal.ID,
		RequestID:       requestIDFrom(r.Context()),
	}
	found, err := a.db.DeleteRelationship(r.Context(), id, actor)
	if err != nil {
		relationshipErrorResponse(w, r, err)
		return
	}
	// Deleting something already gone answers success, not 404: a replayed
	// delete must not look like a failure. See DeleteRelationship's own
	// comment. found is intentionally not distinguished in the response.
	_ = found
	w.WriteHeader(http.StatusNoContent)
}
