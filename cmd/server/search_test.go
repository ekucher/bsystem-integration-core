package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/search"
)

// grantStub answers scope-grant lookups without a database.
type grantStub struct {
	granted map[string]bool
	err     error
}

func (g grantStub) HasScopedPermission(_ context.Context, principalType, principalID, scopeType, scopeID, permission string) (bool, error) {
	if g.err != nil {
		return false, g.err
	}
	return g.granted[principalType+"|"+principalID+"|"+scopeType+"|"+scopeID+"|"+permission], nil
}

func searchApp(grants authz.Grants) *app {
	return &app{authz: authz.New(grants, authz.DefaultConfinedRoles()), searchProvider: search.NewMemory()}
}

func hit(id, scopeType, scopeID string, permissions ...string) search.Hit {
	return search.Hit{Document: search.Document{
		ID: id, Type: search.TypeClient, Title: id, Source: "test",
		Permissions: permissions, ScopeType: scopeType, ScopeID: scopeID,
	}}
}

// Search is the endpoint most likely to disclose something by accident: it
// returns records the caller never named, from every module at once. These
// cases are the boundary.
func TestSearchResultsAreAuthorizedIndividually(t *testing.T) {
	candidates := []search.Hit{
		hit("CL-000001", authz.ScopeClient, "CL-000001", "crm.client.read"),
		hit("PR-000001", authz.ScopeProject, "PR-000001", "projects.task.read"),
		hit("DOC-000001", authz.ScopeResource, "DOC-000001", "wiki.document.read"),
	}

	cases := []struct {
		name      string
		principal authz.Principal
		grants    authz.Grants
		want      []string
	}{
		{
			name:      "an administrator sees everything",
			principal: authz.Principal{ID: "USR-1", Kind: authz.KindUser, Roles: []string{"Administrator"}, Permissions: []string{"*"}},
			want:      []string{"CL-000001", "PR-000001", "DOC-000001"},
		},
		{
			name:      "a role sees only what its permissions cover",
			principal: authz.Principal{ID: "USR-2", Kind: authz.KindUser, Roles: []string{"Developer"}, Permissions: []string{"projects.task.read", "wiki.document.read"}},
			want:      []string{"PR-000001", "DOC-000001"},
		},
		{
			// This is the case the whole design exists for. A customer's
			// crm.client.read means "my own record", not "clients". Without
			// per-document evaluation a search would hand them every client
			// in the platform.
			name:      "a scope-confined role sees nothing without a grant",
			principal: authz.Principal{ID: "USR-3", Kind: authz.KindUser, Roles: []string{"Customer"}, Permissions: []string{"crm.client.read", "wiki.document.read"}},
			want:      nil,
		},
		{
			name:      "a scope-confined role sees exactly what it was granted",
			principal: authz.Principal{ID: "USR-4", Kind: authz.KindUser, Roles: []string{"Customer"}, Permissions: []string{"crm.client.read", "wiki.document.read"}},
			grants:    grantStub{granted: map[string]bool{"user|USR-4|client|CL-000001|crm.client.read": true}},
			want:      []string{"CL-000001"},
		},
		{
			name:      "a principal whose groups mapped to nothing sees nothing",
			principal: authz.Principal{ID: "USR-5", Kind: authz.KindUser},
			want:      nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := searchApp(c.grants)
			r := httptest.NewRequest("GET", "/api/v1/search?q=x", nil)
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey, c.principal))

			hits, consumed, err := a.permittedHits(r, candidates, 10)
			if err != nil {
				t.Fatalf("permittedHits: %v", err)
			}
			if consumed != len(candidates) {
				t.Errorf("consumed = %d, want every candidate examined", consumed)
			}
			got := make([]string, 0, len(hits))
			for _, h := range hits {
				got = append(got, h.ID)
			}
			if len(got) != len(c.want) {
				t.Fatalf("hits = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("hits = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// A document may name several permissions; holding any one is enough, and
// holding none is not.
func TestAnyOfTheDocumentsPermissionsIsEnough(t *testing.T) {
	candidate := hit("PR-000001", authz.ScopeProject, "PR-000001", "projects.task.read", "development.repo.read")
	a := searchApp(nil)

	for name, c := range map[string]struct {
		permissions []string
		want        bool
	}{
		"the first":  {[]string{"projects.task.read"}, true},
		"the second": {[]string{"development.repo.read"}, true},
		"both":       {[]string{"projects.task.read", "development.repo.read"}, true},
		"neither":    {[]string{"crm.client.read"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/search?q=x", nil)
			principal := authz.Principal{ID: "USR-1", Kind: authz.KindUser, Roles: []string{"Developer"}, Permissions: c.permissions}
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey, principal))
			allowed, err := a.mayRead(r, principal, candidate)
			if err != nil {
				t.Fatalf("mayRead: %v", err)
			}
			if allowed != c.want {
				t.Errorf("allowed = %v, want %v", allowed, c.want)
			}
		})
	}
}

// A document carrying no permissions is visible to nobody — including an
// administrator. Indexing is supposed to refuse one, so reaching here means
// something upstream let it through, and the safe reading of "no answer" is
// not "everyone".
func TestADocumentWithNoPermissionsIsVisibleToNobody(t *testing.T) {
	a := searchApp(nil)
	r := httptest.NewRequest("GET", "/api/v1/search?q=x", nil)
	principal := authz.Principal{ID: "USR-1", Kind: authz.KindUser, Roles: []string{"Administrator"}, Permissions: []string{"*"}}
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey, principal))

	allowed, err := a.mayRead(r, principal, hit("CL-000001", authz.ScopeClient, "CL-000001"))
	if err != nil {
		t.Fatalf("mayRead: %v", err)
	}
	if allowed {
		t.Error("a document with no declared audience was returned")
	}
}

// A scope lookup that fails means the decision could not be made. Returning a
// shorter list would present an incomplete result as a complete one.
func TestAFailedScopeLookupFailsTheRequest(t *testing.T) {
	a := searchApp(grantStub{err: errors.New("scope store unavailable")})
	r := httptest.NewRequest("GET", "/api/v1/search?q=x", nil)
	principal := authz.Principal{ID: "USR-3", Kind: authz.KindUser, Roles: []string{"Customer"}, Permissions: []string{"crm.client.read"}}
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey, principal))

	if _, _, err := a.permittedHits(r, []search.Hit{hit("CL-000001", authz.ScopeClient, "CL-000001", "crm.client.read")}, 10); err == nil {
		t.Error("a failed authorization lookup was treated as a denial and the request succeeded")
	}
}

// The filter must stop once the page is full, so a caller asking for two
// results does not pay for a hundred authorization decisions — and the count
// it reports must let the next cursor resume without skipping the candidates
// it never looked at.
func TestFilteringStopsOnceThePageIsFull(t *testing.T) {
	candidates := []search.Hit{
		hit("CL-000001", authz.ScopeClient, "CL-000001", "crm.client.read"),
		hit("CL-000002", authz.ScopeClient, "CL-000002", "crm.client.read"),
		hit("CL-000003", authz.ScopeClient, "CL-000003", "crm.client.read"),
	}
	a := searchApp(nil)
	r := httptest.NewRequest("GET", "/api/v1/search?q=x", nil)
	principal := authz.Principal{ID: "USR-1", Kind: authz.KindUser, Roles: []string{"Support"}, Permissions: []string{"crm.client.read"}}
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey, principal))

	hits, consumed, err := a.permittedHits(r, candidates, 2)
	if err != nil {
		t.Fatalf("permittedHits: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want the requested page size", len(hits))
	}
	if consumed != 2 {
		t.Errorf("consumed = %d, want only the candidates actually examined: the rest must be reachable from the next cursor", consumed)
	}
}
