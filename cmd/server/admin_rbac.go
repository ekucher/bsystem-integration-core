package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

func (a *app) adminRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := a.db.ListRoles(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "RBAC catalog unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, roles)
}

func (a *app) adminListScopes(w http.ResponseWriter, r *http.Request) {
	principalType := strings.TrimSpace(r.URL.Query().Get("principal_type"))
	principalID := strings.TrimSpace(r.URL.Query().Get("principal_id"))
	if principalType == "" || principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "principal_type and principal_id are required"})
		return
	}
	grants, err := a.db.ListScopeGrants(r.Context(), principalType, principalID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scope store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func decodeScopeGrant(w http.ResponseWriter, r *http.Request) (platformdb.ScopeGrant, bool) {
	var grant platformdb.ScopeGrant
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&grant); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return platformdb.ScopeGrant{}, false
	}
	grant.PrincipalType = strings.TrimSpace(grant.PrincipalType)
	grant.PrincipalID = strings.TrimSpace(grant.PrincipalID)
	grant.ScopeType = strings.TrimSpace(grant.ScopeType)
	grant.ScopeID = strings.TrimSpace(grant.ScopeID)
	grant.PermissionID = strings.TrimSpace(grant.PermissionID)
	if grant.PrincipalType == "" || grant.PrincipalID == "" || grant.ScopeType == "" || grant.ScopeID == "" || grant.PermissionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "all scope grant fields are required"})
		return platformdb.ScopeGrant{}, false
	}
	return grant, true
}

func (a *app) adminAddScope(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	grant, ok := decodeScopeGrant(w, r)
	if !ok {
		return
	}
	if err := a.db.AddScopeGrant(r.Context(), grant); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.audit(r, access, "rbac.scope.granted", grant.ScopeType, grant.ScopeID, map[string]any{"principal_type": grant.PrincipalType, "principal_id": grant.PrincipalID, "permission": grant.PermissionID})
	writeJSON(w, http.StatusCreated, grant)
}

func (a *app) adminDeleteScope(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	grant, ok := decodeScopeGrant(w, r)
	if !ok {
		return
	}
	if err := a.db.DeleteScopeGrant(r.Context(), grant); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scope store unavailable"})
		return
	}
	a.audit(r, access, "rbac.scope.revoked", grant.ScopeType, grant.ScopeID, map[string]any{"principal_type": grant.PrincipalType, "principal_id": grant.PrincipalID, "permission": grant.PermissionID})
	writeJSON(w, http.StatusOK, grant)
}
