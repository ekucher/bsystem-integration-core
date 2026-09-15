package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
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

// adapterResilience reads the shared upstream resilience policy from the
// environment. The defaults are the production ones; a deployment overrides
// them when its upstreams behave differently, and the E2E stack shortens the
// circuit window so recovery is observable within a test run.
func adapterResilience() adapters.Config {
	return adapters.Config{
		Recorder:                upstream,
		Timeout:                 durationEnv("ADAPTER_TIMEOUT", 10*time.Second),
		RetryAttempts:           intEnv("ADAPTER_RETRY_ATTEMPTS", 0),
		CircuitFailureThreshold: intEnv("ADAPTER_CIRCUIT_FAILURE_THRESHOLD", 0),
		CircuitOpenFor:          durationEnv("ADAPTER_CIRCUIT_OPEN_FOR", 0),
	}
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func intEnv(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return value
}

// disabledAdapter registers a placeholder so that an unconfigured upstream is
// reported as disabled rather than being absent: the registry always
// describes the whole intended surface.
func disabledAdapter(id, name string, capabilities []string) adapters.Mock {
	return adapters.Mock{
		AdapterInfo:   adapters.Info{ID: id, Name: name, Version: "0", Status: adapters.StatusDisabled, Capabilities: capabilities},
		AdapterHealth: adapters.Health{Status: adapters.StatusDisabled, Message: "not configured"},
	}
}

func init() {
	resilience := adapterResilience()

	config := resilience
	config.BaseURL, config.APIKey = strings.TrimSpace(os.Getenv("ESPOCRM_URL")), os.Getenv("ESPOCRM_API_KEY")
	if config.BaseURL != "" {
		if client, err := espocrm.New(config); err != nil {
			logger.Warn("adapter disabled", "adapter", "espocrm", "error", err.Error())
		} else {
			_ = adapterRegistry.Register(client)
		}
	}
	if _, registered := adapterRegistry.Get("espocrm"); !registered {
		_ = adapterRegistry.Register(disabledAdapter("espocrm", "EspoCRM", []string{"clients.read", "contacts.read"}))
	}

	config = resilience
	config.BaseURL, config.APIKey = strings.TrimSpace(os.Getenv("REDMINE_URL")), os.Getenv("REDMINE_API_KEY")
	if config.BaseURL != "" {
		if client, err := redmine.New(config); err != nil {
			logger.Warn("adapter disabled", "adapter", "redmine", "error", err.Error())
		} else {
			_ = adapterRegistry.Register(client)
		}
	}
	if _, registered := adapterRegistry.Get("redmine"); !registered {
		_ = adapterRegistry.Register(disabledAdapter("redmine", "Redmine", []string{"projects.read", "issues.read"}))
	}

	config = resilience
	config.BaseURL, config.APIKey = strings.TrimSpace(os.Getenv("OUTLINE_URL")), os.Getenv("OUTLINE_API_KEY")
	if config.BaseURL != "" {
		if client, err := outline.New(config); err != nil {
			logger.Warn("adapter disabled", "adapter", "outline", "error", err.Error())
		} else {
			_ = adapterRegistry.Register(client)
		}
	}
	if _, registered := adapterRegistry.Get("outline"); !registered {
		_ = adapterRegistry.Register(disabledAdapter("outline", "Outline", []string{"documents.read", "documents.search"}))
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
			logger.WarnContext(r.Context(), "service authentication failed", "error", err.Error())
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
			logger.ErrorContext(r.Context(), "service identity persistence failed", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service identity persistence unavailable"})
			return
		}
		if created {
			a.publish("service_identity.created", map[string]any{"global_service_id": globalID, "subject": info.Sub})
		}
		profile, err := a.db.ResolveAccess(r.Context(), groups, "service")
		if err != nil {
			logger.ErrorContext(r.Context(), "service RBAC resolution failed", "error", err.Error())
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
