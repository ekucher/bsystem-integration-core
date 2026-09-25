package main

import (
	"net/http"
	"sort"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
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
	Method string
	Path   string
	Auth   authKind
	// Permission is required to reach the handler. An empty permission means
	// authentication alone is enough; "*" means administrator.
	Permission string
	// ResourceScope, when set, means the route addresses a single resource.
	// The permission is then evaluated in the handler against the resolved
	// resource rather than across the platform, so a scope-confined role can
	// reach exactly what it has been granted and nothing else.
	ResourceScope string
	// Class overrides the rate-limit bucket this route falls into. Empty
	// means it is derived from Auth, so a route added here is limited by
	// default rather than unlimited until somebody remembers it.
	Class   limitClass
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
		{Method: http.MethodGet, Path: "/api/v1/clients", Auth: authHuman, Permission: "crm.client.read", Handler: func(a *app) http.HandlerFunc { return a.listClients }},
		{Method: http.MethodGet, Path: "/api/v1/clients/{id}", Auth: authHuman, Permission: "crm.client.read", ResourceScope: authz.ScopeClient, Handler: func(a *app) http.HandlerFunc { return a.getClient }},
		{Method: http.MethodGet, Path: "/api/v1/contacts", Auth: authHuman, Permission: "crm.client.read", Handler: func(a *app) http.HandlerFunc { return a.listContacts }},
		{Method: http.MethodGet, Path: "/api/v1/contacts/{id}", Auth: authHuman, Permission: "crm.client.read", ResourceScope: authz.ScopeResource, Handler: func(a *app) http.HandlerFunc { return a.getContact }},
		{Method: http.MethodGet, Path: "/api/v1/projects", Auth: authHuman, Permission: "projects.task.read", Handler: func(a *app) http.HandlerFunc { return a.listProjects }},
		{Method: http.MethodGet, Path: "/api/v1/projects/{id}", Auth: authHuman, Permission: "projects.task.read", ResourceScope: authz.ScopeProject, Handler: func(a *app) http.HandlerFunc { return a.getProject }},
		{Method: http.MethodGet, Path: "/api/v1/issues", Auth: authHuman, Permission: "projects.task.read", Handler: func(a *app) http.HandlerFunc { return a.listIssues }},
		{Method: http.MethodGet, Path: "/api/v1/issues/{id}", Auth: authHuman, Permission: "projects.task.read", ResourceScope: authz.ScopeResource, Handler: func(a *app) http.HandlerFunc { return a.getIssue }},
		{Method: http.MethodGet, Path: "/api/v1/documents", Auth: authHuman, Permission: "wiki.document.read", Handler: func(a *app) http.HandlerFunc { return a.listDocuments }},
		{Method: http.MethodGet, Path: "/api/v1/documents/{id}", Auth: authHuman, Permission: "wiki.document.read", ResourceScope: authz.ScopeResource, Handler: func(a *app) http.HandlerFunc { return a.getDocument }},
		{Method: http.MethodPost, Path: "/api/v1/ai/ask", Auth: authHuman, Permission: "ai.query", Class: limitAI, Handler: func(a *app) http.HandlerFunc { return a.aiAsk }},
		{Method: http.MethodGet, Path: "/api/v1/ai/audit", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.aiAudit }},
		{Method: http.MethodGet, Path: "/api/v1/incidents", Auth: authHuman, Permission: "support.incident.read", Handler: func(a *app) http.HandlerFunc { return a.listSupportRecords }},
		{Method: http.MethodPost, Path: "/api/v1/incidents", Auth: authHuman, Permission: "support.incident.write", Handler: func(a *app) http.HandlerFunc { return a.createSupportRecord }},
		{Method: http.MethodGet, Path: "/api/v1/incidents/{id}", Auth: authHuman, Permission: "support.incident.read", ResourceScope: authz.ScopeClient, Handler: func(a *app) http.HandlerFunc { return a.getSupportRecord }},
		{Method: http.MethodPatch, Path: "/api/v1/incidents/{id}", Auth: authHuman, Permission: "support.incident.write", ResourceScope: authz.ScopeClient, Handler: func(a *app) http.HandlerFunc { return a.updateSupportRecord }},
		{Method: http.MethodGet, Path: "/api/v1/servers", Auth: authHuman, Permission: "operations.server.read", Handler: func(a *app) http.HandlerFunc { return a.listServers }},
		{Method: http.MethodGet, Path: "/api/v1/servers/{id}", Auth: authHuman, Permission: "operations.server.read", ResourceScope: authz.ScopeClient, Handler: func(a *app) http.HandlerFunc { return a.getServer }},
		{Method: http.MethodGet, Path: "/api/v1/operations/events", Auth: authHuman, Permission: "operations.server.read", Handler: func(a *app) http.HandlerFunc { return a.listOperationsEvents }},
		{Method: http.MethodPost, Path: "/api/v1/global-ids", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.createGlobalID }},
		{Method: http.MethodGet, Path: "/api/v1/global-ids/{id}", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.resolveGlobalID }},
		{Method: http.MethodGet, Path: "/api/v1/global-ids/{id}/relationships", Auth: authHuman, Permission: "relationships.read", Handler: func(a *app) http.HandlerFunc { return a.listGlobalIDRelationships }},
		// Search carries no route permission: what a caller may see is decided
		// per document, and a route permission could only be broader than
		// that filter. See cmd/server/search.go.
		{Method: http.MethodGet, Path: "/api/v1/search", Auth: authHuman, Class: limitSearch, Handler: func(a *app) http.HandlerFunc { return a.search }},

		// Notifications carry no route permission: what a caller may read is
		// decided per notification from their own audience, so a permission
		// here would either be too broad to mean anything or would hide
		// notifications addressed to them by name.
		{Method: http.MethodGet, Path: "/api/v1/notifications", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.listNotifications }},
		{Method: http.MethodPost, Path: "/api/v1/notifications/{id}/read", Auth: authHuman, Handler: func(a *app) http.HandlerFunc { return a.markNotificationRead }},
		{Method: http.MethodGet, Path: "/api/v1/audit", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.auditEvents }},
		{Method: http.MethodGet, Path: "/api/v1/admin/users", Auth: authHuman, Permission: "identity.user.read", Handler: func(a *app) http.HandlerFunc { return a.adminUsers }},
		{Method: http.MethodGet, Path: "/api/v1/admin/accounts", Auth: authHuman, Permission: "identity.user.read", Handler: func(a *app) http.HandlerFunc { return a.adminAccounts }},
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts", Auth: authHuman, Permission: "identity.user.manage", Handler: func(a *app) http.HandlerFunc { return a.adminCreateAccount }},
		{Method: http.MethodPatch, Path: "/api/v1/admin/accounts/{id}", Auth: authHuman, Permission: "identity.user.manage", Handler: func(a *app) http.HandlerFunc { return a.adminUpdateAccount }},
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts/{id}/password", Auth: authHuman, Permission: "identity.user.manage", Handler: func(a *app) http.HandlerFunc { return a.adminSetAccountPassword }},
		{Method: http.MethodGet, Path: "/api/v1/admin/modules", Auth: authHuman, Permission: "module.admin", Handler: func(a *app) http.HandlerFunc { return a.adminModules }},
		{Method: http.MethodPost, Path: "/api/v1/admin/modules", Auth: authHuman, Permission: "module.admin", Handler: func(a *app) http.HandlerFunc { return a.adminCreateModule }},
		{Method: http.MethodGet, Path: "/api/v1/admin/modules/allowed-origins", Auth: authHuman, Permission: "module.admin", Handler: func(a *app) http.HandlerFunc { return a.adminModuleAllowedOrigins }},
		{Method: http.MethodPatch, Path: "/api/v1/admin/modules/{id}", Auth: authHuman, Permission: "module.admin", Handler: func(a *app) http.HandlerFunc { return a.adminUpdateModule }},
		{Method: http.MethodGet, Path: "/api/v1/admin/rbac/roles", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.adminRoles }},
		{Method: http.MethodGet, Path: "/api/v1/admin/rbac/scopes", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.adminListScopes }},
		{Method: http.MethodPost, Path: "/api/v1/admin/rbac/scopes", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.adminAddScope }},
		{Method: http.MethodDelete, Path: "/api/v1/admin/rbac/scopes", Auth: authHuman, Permission: "*", Handler: func(a *app) http.HandlerFunc { return a.adminDeleteScope }},

		// Machine API. Kept separate from the human API so that a human token
		// can never reach it and vice versa.
		{Method: http.MethodGet, Path: "/api/service/v1/whoami", Auth: authService, Handler: func(a *app) http.HandlerFunc { return a.serviceWhoAmI }},
		{Method: http.MethodGet, Path: "/api/service/v1/adapters", Auth: authService, Permission: "adapters.read", Handler: func(a *app) http.HandlerFunc { return a.serviceAdapters }},
		{Method: http.MethodGet, Path: "/api/service/v1/adapters/health", Auth: authService, Permission: "adapters.read", Handler: func(a *app) http.HandlerFunc { return a.serviceAdapterHealth }},
		{Method: http.MethodPost, Path: "/api/service/v1/events", Auth: authService, Permission: "events.publish", Handler: func(a *app) http.HandlerFunc { return a.servicePublishEvent }},
		{Method: http.MethodPost, Path: "/api/service/v1/notifications", Auth: authService, Permission: "notifications.publish", Handler: func(a *app) http.HandlerFunc { return a.servicePublishNotification }},
		{Method: http.MethodPost, Path: "/api/service/v1/servers", Auth: authService, Permission: "operations.report", Handler: func(a *app) http.HandlerFunc { return a.serviceRegisterServer }},
		{Method: http.MethodPost, Path: "/api/service/v1/operations/events", Auth: authService, Permission: "operations.report", Handler: func(a *app) http.HandlerFunc { return a.serviceReportOperationsEvent }},
		{Method: http.MethodPost, Path: "/api/service/v1/search/documents", Auth: authService, Permission: "search.index", Handler: func(a *app) http.HandlerFunc { return a.serviceIndexSearchDocuments }},
		{Method: http.MethodDelete, Path: "/api/service/v1/search/documents/{id}", Auth: authService, Permission: "search.index", Handler: func(a *app) http.HandlerFunc { return a.serviceDeleteSearchDocument }},
		// Relationships require a service identity plus a verified end-user
		// on-behalf-of token; see cmd/server/relationships.go for why a raw
		// header field is never trusted as the end user's identity.
		{Method: http.MethodPost, Path: "/api/service/v1/relationships", Auth: authService, Permission: "relationships.write", Handler: func(a *app) http.HandlerFunc { return a.serviceCreateRelationship }},
		{Method: http.MethodGet, Path: "/api/service/v1/relationships", Auth: authService, Permission: "relationships.read", Handler: func(a *app) http.HandlerFunc { return a.serviceListRelationships }},
		{Method: http.MethodDelete, Path: "/api/service/v1/relationships/{id}", Auth: authService, Permission: "relationships.write", Handler: func(a *app) http.HandlerFunc { return a.serviceDeleteRelationship }},
	}
}

// handler builds the server handler from the route inventory. Authentication
// and authorization are applied from each route's declared boundary and
// permission rather than from per-route wiring, so a new route cannot
// accidentally be registered unauthenticated or unauthorized.
func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	observer := a.observer()
	for _, r := range routes() {
		handler := http.Handler(r.Handler(a))
		// Authorization wraps the handler first so that it runs after
		// authentication has resolved the principal. A route that addresses a
		// single resource evaluates in the handler instead, once it knows
		// which resource was addressed.
		if r.Auth != authNone && r.ResourceScope == "" {
			handler = a.authorize(r.Permission, handler)
		}
		// The limit sits between the two: after authentication, because it is
		// counted against the authenticated principal, and before
		// authorization, because refusing early is the cheaper half of the
		// point.
		handler = a.limit(classOf(r), handler)
		switch r.Auth {
		case authHuman:
			handler = a.authenticate(handler)
		case authService:
			handler = a.authenticateService(handler)
		}
		// Logging and metrics wrap the outside, so they observe the request
		// even when authentication or authorization refuses it. A rejected
		// request is exactly the one an operator needs to see.
		mux.Handle(r.Pattern(), observer.Observe(r.Pattern(), handler))
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
