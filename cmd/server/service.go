package main

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

type servicePrincipal struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Name        string   `json:"name"`
	Username    string   `json:"username"`
	Groups      []string `json:"groups"`
	Permissions []string `json:"permissions"`
}

const (
	serviceContextKey contextKey = "service"
	serviceGroup                 = "BSYSTEM-Services"
)

var adapterRegistry = adapters.NewRegistry()

func init() {
	planned := []adapters.Mock{
		{AdapterInfo: adapters.Info{ID: "espocrm", Name: "EspoCRM", Version: "0", Status: adapters.StatusDisabled, Capabilities: []string{"clients.read", "contacts.read"}}, AdapterHealth: adapters.Health{Status: adapters.StatusDisabled, Message: "not configured"}},
		{AdapterInfo: adapters.Info{ID: "redmine", Name: "Redmine", Version: "0", Status: adapters.StatusDisabled, Capabilities: []string{"projects.read", "issues.read"}}, AdapterHealth: adapters.Health{Status: adapters.StatusDisabled, Message: "not configured"}},
		{AdapterInfo: adapters.Info{ID: "outline", Name: "Outline", Version: "0", Status: adapters.StatusDisabled, Capabilities: []string{"documents.read"}}, AdapterHealth: adapters.Health{Status: adapters.StatusDisabled, Message: "not configured"}},
	}
	for _, adapter := range planned {
		_ = adapterRegistry.Register(adapter)
	}
}

func hasString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (a *app) authenticateService(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}

		info, err := fetchUserInfo(r.Context(), strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			log.Printf("service authentication failed: %v", err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired token"})
			return
		}
		if !hasString(info.Groups, serviceGroup) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "service identity group required"})
			return
		}

		username := info.PreferredUsername
		if username == "" {
			username = info.Email
		}
		name := info.Name
		if name == "" {
			name = username
		}

		globalID, created, err := a.db.EnsureServiceIdentity(r.Context(), platformdb.ServiceIdentity{
			Subject: info.Sub,
			Name: name,
			Username: username,
			Groups: unique(info.Groups),
		})
		if err != nil {
			log.Printf("service identity persistence failed: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service identity persistence unavailable"})
			return
		}
		if created {
			a.publish("service_identity.created", map[string]any{"global_service_id": globalID, "subject": info.Sub})
		}

		principal := servicePrincipal{
			ID: globalID,
			Subject: info.Sub,
			Name: name,
			Username: username,
			Groups: unique(info.Groups),
			Permissions: []string{"adapters.read", "events.publish", "global_ids.read"},
		}
		ctx := context.WithValue(r.Context(), serviceContextKey, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func serviceFrom(ctx context.Context) servicePrincipal {
	principal, _ := ctx.Value(serviceContextKey).(servicePrincipal)
	return principal
}

func (a *app) serviceWhoAmI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, serviceFrom(r.Context()))
}

func (a *app) serviceAdapters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, adapterRegistry.List())
}

func (a *app) serviceAdapterHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, adapterRegistry.Health(r.Context()))
}

func registerServiceRoutes(root *http.ServeMux, a *app) {
	serviceMux := http.NewServeMux()
	serviceMux.HandleFunc("GET /api/service/v1/whoami", a.serviceWhoAmI)
	serviceMux.HandleFunc("GET /api/service/v1/adapters", a.serviceAdapters)
	serviceMux.HandleFunc("GET /api/service/v1/adapters/health", a.serviceAdapterHealth)
	root.Handle("/api/service/", a.authenticateService(serviceMux))
}
