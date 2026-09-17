package main

import "net/http"

// adminUsers returns the persistent platform identities that have successfully
// authenticated at least once. authentik remains the source of credentials and
// account activation; this endpoint deliberately exposes only Integration
// Core-owned identity state and immutable Global User IDs.
func (a *app) adminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.db.ListIdentities(r.Context())
	if err != nil {
		logger.ErrorContext(r.Context(), "identity directory read failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":      "identity directory unavailable",
			"code":       "identity_store_unavailable",
			"request_id": requestIDFrom(r.Context()),
		})
		return
	}
	writeJSON(w, http.StatusOK, users)
}
