package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/search"
)

// searchCollection is the search envelope.
//
// It deliberately has no total. The provider knows how many documents matched
// before the platform applied its authorization filter, and that number
// counts records the caller may not be allowed to know exist: a customer
// searching for a competitor's name and being told "47 results" has learned
// something, whatever the page then shows. A caller walks the results by
// following the cursor, which is all a total would have been used for.
type searchCollection struct {
	Data       []search.Hit         `json:"data"`
	Pagination searchPaginationView `json:"pagination"`
}

type searchPaginationView struct {
	Limit int `json:"limit"`
	// NextCursor is absent once the results are exhausted. It can be present
	// on a page shorter than the limit, because the authorization filter
	// removes candidates after the provider has counted them.
	NextCursor string `json:"next_cursor,omitempty"`
}

// candidateFactor is how many candidates the platform asks the provider for
// per result it intends to return, since some will be filtered out. The
// absolute cap is what stops one search from scanning an unbounded slice of
// the index on behalf of a caller who can see almost none of it.
const (
	candidateFactor = 4
	candidateCap    = 100
)

func (a *app) search(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())

	text := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(text)) < search.MinQueryLength {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "q must be at least " + strconv.Itoa(search.MinQueryLength) + " characters",
			"code":  "invalid_query",
		})
		return
	}
	types, err := search.NormalizeTypes(r.URL.Query()["type"])
	if err != nil {
		// An unknown type is refused rather than ignored: dropping it would
		// answer a broader question than the caller asked and they would read
		// the result as though it had been filtered.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_type"})
		return
	}
	offset, err := search.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pagination cursor", "code": "invalid_cursor"})
		return
	}
	limit, convErr := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if convErr != nil {
		limit = 0
	}
	limit = search.ClampLimit(limit)

	candidates := limit * candidateFactor
	if candidates > candidateCap {
		candidates = candidateCap
	}

	page, err := a.searchProvider.Search(r.Context(), search.Query{
		Text:           text,
		Types:          types,
		Permissions:    access.Permissions,
		AllPermissions: hasString(access.Permissions, authz.PermissionAll),
		Offset:         offset,
		Limit:          candidates,
	})
	if err != nil {
		// "The engine is down" must never be rendered as "no results": a user
		// told their search found nothing will act on it.
		logger.ErrorContext(r.Context(), "search failed", "provider", a.searchProvider.Name(), "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "search provider unavailable", "code": "search_unavailable",
		})
		return
	}

	hits, consumed, err := a.permittedHits(r, page.Hits, limit)
	if err != nil {
		logger.ErrorContext(r.Context(), "authorization evaluation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization store unavailable"})
		return
	}

	next := ""
	if offset+consumed < page.Total {
		next = search.EncodeCursor(offset + consumed)
	}
	writeJSON(w, http.StatusOK, searchCollection{
		Data:       hits,
		Pagination: searchPaginationView{Limit: len(hits), NextCursor: next},
	})
}

// permittedHits applies the platform's authorization rules to search
// candidates and reports how many were examined.
//
// The provider already narrowed by permission, and this runs anyway. That is
// not redundancy for its own sake: the provider's narrowing is a set
// intersection over a field in an index, while authorization is a decision
// that also depends on scope grants and on whether the principal's role is
// confined. A provider cannot make that decision, and a search endpoint is
// the last place to let one try.
func (a *app) permittedHits(r *http.Request, candidates []search.Hit, limit int) ([]search.Hit, int, error) {
	principal := principalFrom(r.Context())
	permitted := make([]search.Hit, 0, limit)
	consumed := 0

	for _, hit := range candidates {
		consumed++
		allowed, err := a.mayRead(r, principal, hit)
		if err != nil {
			return nil, 0, err
		}
		if allowed {
			permitted = append(permitted, hit)
		}
		if len(permitted) == limit {
			break
		}
	}
	return permitted, consumed, nil
}

// mayRead answers whether a principal may see one document. Any one of the
// document's permissions is enough; a document carrying none is visible to
// nobody, which is the direction an unanswered question should fail in.
func (a *app) mayRead(r *http.Request, principal authz.Principal, hit search.Hit) (bool, error) {
	for _, permission := range hit.Permissions {
		decision, err := a.authz.Evaluate(r.Context(), principal, permission, authz.Resource(hit.ScopeType, hit.ScopeID))
		if err != nil {
			// A lookup failure is not a denial to be swallowed: it means the
			// decision could not be made, and the request has to fail rather
			// than quietly return a shorter list.
			return false, err
		}
		if decision.Allowed {
			return true, nil
		}
	}
	return false, nil
}

// searchDocumentsRequest is the machine API's indexing body.
type searchDocumentsRequest struct {
	Documents []search.Document `json:"documents"`
}

func (a *app) serviceIndexSearchDocuments(w http.ResponseWriter, r *http.Request) {
	var request searchDocumentsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if len(request.Documents) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one document is required"})
		return
	}
	if err := a.searchProvider.Index(r.Context(), request.Documents...); err != nil {
		if errors.Is(err, search.ErrInvalidDocument) {
			// The validation message names the field, never the document, so
			// a rejection cannot echo indexed content back to a publisher who
			// guessed at it.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_document"})
			return
		}
		logger.ErrorContext(r.Context(), "search indexing failed", "provider", a.searchProvider.Name(), "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search provider unavailable", "code": "search_unavailable"})
		return
	}
	searchIndexed.Inc("index", "ok")
	writeJSON(w, http.StatusAccepted, map[string]int{"indexed": len(request.Documents)})
}

func (a *app) serviceDeleteSearchDocument(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a Global ID is required"})
		return
	}
	if err := a.searchProvider.Delete(r.Context(), id); err != nil {
		logger.ErrorContext(r.Context(), "search delete failed", "provider", a.searchProvider.Name(), "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search provider unavailable", "code": "search_unavailable"})
		return
	}
	searchIndexed.Inc("delete", "ok")
	// Removing a document that was never indexed succeeds. The caller's
	// intent — "this should not be searchable" — is satisfied either way, and
	// reporting 404 would make a replayed delete look like a failure.
	w.WriteHeader(http.StatusNoContent)
}
