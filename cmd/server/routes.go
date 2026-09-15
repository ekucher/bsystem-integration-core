package main

import (
	"net/http"
	"sort"
)

// authKind is the authentication boundary a route sits behind.
type authKind string

const (
	// authNone is reachable without a token. Only operational endpoints that
	// disclose no business data may use it.
	authNone authKind = "none"
	// authHuman requires a human bearer token resolved through authentik.
	authHuman authKind = "human"
	// authService requires a service identity in the BSYSTEM-Services group.
	authService authKind = "service"
)

// route is one endpoint the server exposes.
type route struct {
	Method  string
	Path    string
	Auth    authKind
	Handler func(*app) http.HandlerFunc
}

// Pattern renders the route as a net/http routing pattern.
func (r route) Pattern() string { return r.Method + " " + r.Path }

// routes is the authoritative inventory of every endpoint the server serves.
//
// docs/openapi.yaml is contract-tested against this list, so a route added
// here without a matching documented operation fails CI, and a documented
// operation with no route fails it too.
func routes() []route {
	return []route{
		// Operational endpoints. These are unauthenticated by design and must
		// never disclose business data, credentials or internal topology.
		{Method: http.MethodGet, Path: "/health", Auth: authNone, Handler: func(a *app) http.HandlerFunc { return a.health }},
		{Method: http.MethodGet, Path: "/readyz", Auth: authNone, Handler: func(a *app) http.HandlerFunc { return a.readiness }},
		{Method: http.MethodGet, Path: "/metrics", Auth: authNone, Handler: func(a *app) http.HandlerFunc { return a.metrics }},

		// Human API.
		{Method: http.MethodGet, Path: "/api/v1/me", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.me }},
		{Method: http.MethodGet, Path: "/api/v1/modules", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.modules }},
		{Method: http.MethodGet, Path: "/api/v1/clients", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.listClients }},
		{Method: http.MethodGet, Path: "/api/v1/contacts", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.listContacts }},
		{Method: http.MethodGet, Path: "/api/v1/projects", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.listProjects }},
		{Method: http.MethodGet, Path: "/api/v1/issues", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.listIssues }},
		{Method: http.MethodGet, Path: "/api/v1/documents", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.listDocuments }},
		{Method: http.MethodPost, Path: "/api/v1/global-ids", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.createGlobalID }},
		{Method: http.MethodGet, Path: "/api/v1/global-ids/{id}", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.resolveGlobalID }},
		{Method: http.MethodGet, Path: "/api/v1/audit", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.auditEvents }},
		{Method: http.MethodGet, Path: "/api/v1/admin/rbac/roles", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.adminRoles }},
		{Method: http.MethodGet, Path: "/api/v1/admin/rbac/scopes", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.adminListScopes }},
		{Method: http.MethodPost, Path: "/api/v1/admin/rbac/scopes", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.adminAddScope }},
		{Method: http.MethodDelete, Path: "/api/v1/admin/rbac/scopes", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.adminDeleteScope }},

		// Machine API. Kept separate from the human API so that a human token
		// can never reach it and vice versa.
		{Method: http.MethodGet, Path: "/api/service/v1/whoami", Auth: authService, Handler: func(a *app) http.HandlerFunc { return a.serviceWhoAmI }},
		{Method: http.MethodGet, Path: "/api/service/v1/adapters", Auth: authService, Handler: func(a *app) http.HandlerFunc { return a.serviceAdapters }},
		{Method: http.MethodGet, Path: "/api/service/v1/adapters/health", Auth: authService, Handler: func(a *app) http.HandlerFunc { return a.serviceAdapterHealth }},
		{Method: http.MethodPost, Path: "/api/service/v1/events", Auth: authService, Handler: func(a *app) http.HandlerFunc { return a.servicePublishEvent }},
	}
}

// handler builds the server handler from the route inventory. Authentication
// is applied from each route's declared boundary rather than from per-route
// wiring, so a new route cannot accidentally be registered unauthenticated.
func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range routes() {
		handler := http.Handler(r.Handler(a))
		switch r.Auth {
		case authHuman:
			handler = a.authenticate(handler)
		case authService:
			handler = a.authenticateService(handler)
		}
		mux.Handle(r.Pattern(), handler)
	}
	return requestID(mux)
}

// routePatterns returns every registered pattern, sorted, for diagnostics and
// contract tests.
func routePatterns() []string {
	all := routes()
	patterns := make([]string, 0, len(all))
	for _, r := range all {
		patterns = append(patterns, r.Pattern())
	}
	sort.Strings(patterns)
	return patterns
}
