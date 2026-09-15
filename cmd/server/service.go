package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/espocrm"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/outline"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/redmine"
	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/events"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

type servicePrincipal struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Name        string   `json:"name"`
	Username    string   `json:"username"`
	Groups      []string `json:"groups"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
}

const (
	serviceContextKey contextKey = "service"
	serviceGroup                 = "BSYSTEM-Services"
)

var adapterRegistry = adapters.NewRegistry()

func init() {
	if rawURL := strings.TrimSpace(os.Getenv("ESPOCRM_URL")); rawURL != "" {
		client, err := espocrm.New(rawURL, os.Getenv("ESPOCRM_API_KEY"), 10*time.Second)
		if err != nil {
			log.Printf("EspoCRM adapter disabled: %v", err)
		} else {
			_ = adapterRegistry.Register(client)
		}
	} else {
		_ = adapterRegistry.Register(adapters.Mock{AdapterInfo: adapters.Info{ID: "espocrm", Name: "EspoCRM", Version: "0", Status: adapters.StatusDisabled, Capabilities: []string{"clients.read", "contacts.read"}}, AdapterHealth: adapters.Health{Status: adapters.StatusDisabled, Message: "not configured"}})
	}
	if rawURL := strings.TrimSpace(os.Getenv("REDMINE_URL")); rawURL != "" {
		client, err := redmine.New(rawURL, os.Getenv("REDMINE_API_KEY"), 10*time.Second)
		if err != nil {
			log.Printf("Redmine adapter disabled: %v", err)
		} else {
			_ = adapterRegistry.Register(client)
		}
	} else {
		_ = adapterRegistry.Register(adapters.Mock{AdapterInfo: adapters.Info{ID: "redmine", Name: "Redmine", Version: "0", Status: adapters.StatusDisabled, Capabilities: []string{"projects.read", "issues.read"}}, AdapterHealth: adapters.Health{Status: adapters.StatusDisabled, Message: "not configured"}})
	}
	if rawURL := strings.TrimSpace(os.Getenv("OUTLINE_URL")); rawURL != "" {
		client, err := outline.New(rawURL, os.Getenv("OUTLINE_API_KEY"), 10*time.Second)
		if err != nil {
			log.Printf("Outline adapter disabled: %v", err)
		} else {
			_ = adapterRegistry.Register(client)
		}
	} else {
		_ = adapterRegistry.Register(adapters.Mock{AdapterInfo: adapters.Info{ID: "outline", Name: "Outline", Version: "0", Status: adapters.StatusDisabled, Capabilities: []string{"documents.read"}}, AdapterHealth: adapters.Health{Status: adapters.StatusDisabled, Message: "not configured"}})
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
		groups := unique(info.Groups)
		globalID, created, err := a.db.EnsureServiceIdentity(r.Context(), platformdb.ServiceIdentity{Subject: info.Sub, Name: name, Username: username, Groups: groups})
		if err != nil {
			log.Printf("service identity persistence failed: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service identity persistence unavailable"})
			return
		}
		if created {
			a.publish("service_identity.created", map[string]any{"global_service_id": globalID, "subject": info.Sub})
		}
		profile, err := a.db.ResolveAccess(r.Context(), groups, "service")
		if err != nil {
			log.Printf("service RBAC resolution failed: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "RBAC store unavailable"})
			return
		}
		principal := servicePrincipal{ID: globalID, Subject: info.Sub, Name: name, Username: username, Groups: groups, Roles: profile.Roles, Permissions: profile.Permissions}
		ctx := context.WithValue(r.Context(), serviceContextKey, principal)
		ctx = context.WithValue(ctx, principalContextKey, authz.Principal{
			ID: globalID, Kind: authz.KindService, Roles: profile.Roles, Permissions: profile.Permissions,
		})
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

func (a *app) servicePublishEvent(w http.ResponseWriter, r *http.Request) {
	principal := serviceFrom(r.Context())
	var envelope events.Envelope
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&envelope); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if envelope.ActorID == "" {
		envelope.ActorID = principal.ID
	}
	if envelope.RequestID == "" {
		envelope.RequestID = requestIDFrom(r.Context())
	}
	if err := envelope.Normalize(time.Now()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.nc == nil || !a.nc.IsConnected() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "event bus unavailable"})
		return
	}
	a.publish(events.Subject(envelope.Event), envelope)
	writeJSON(w, http.StatusAccepted, envelope)
}
