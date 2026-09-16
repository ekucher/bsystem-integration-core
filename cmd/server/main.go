package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
	"github.com/ekucher/bsystem-integration-core/internal/search"
	"github.com/nats-io/nats.go"
)

type healthResponse struct {
	Status    string            `json:"status"`
	Service   string            `json:"service"`
	Version   string            `json:"version"`
	Timestamp string            `json:"timestamp"`
	Checks    map[string]string `json:"checks"`
}

type userInfo struct {
	Sub               string   `json:"sub"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`
}

type meResponse struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Email       string   `json:"email"`
	Name        string   `json:"name"`
	Username    string   `json:"username"`
	Groups      []string `json:"groups"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	Modules     []string `json:"modules"`
}

type globalIDRequest struct {
	EntityType string         `json:"entity_type"`
	Source     string         `json:"source"`
	SourceID   string         `json:"source_id"`
	TenantID   string         `json:"tenant_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type contextKey string

const (
	// userContextKey holds the resolved meResponse for a human caller.
	userContextKey contextKey = "user"
	// principalContextKey holds the authorization principal, whether the
	// caller is a human or a service identity.
	principalContextKey contextKey = "principal"
	requestIDContextKey contextKey = "request-id"
)

type app struct {
	db    *platformdb.DB
	nc    *nats.Conn
	authz *authz.Evaluator
	// searchProvider is the search engine. It is always non-nil: the
	// in-memory provider is the default, so search answers honestly with an
	// empty index rather than failing as unconfigured.
	searchProvider search.Provider
}

// accessFrom returns the resolved access of the human caller.
func accessFrom(ctx context.Context) meResponse {
	access, _ := ctx.Value(userContextKey).(meResponse)
	return access
}

// principalFrom returns the authorization principal of the caller.
func principalFrom(ctx context.Context) authz.Principal {
	principal, _ := ctx.Value(principalContextKey).(authz.Principal)
	return principal
}

// writeDenied renders an authorization denial. The decision's reason names the
// missing permission and nothing else, so a denial never reveals whether the
// resource exists or who owns it.
func writeDenied(w http.ResponseWriter, decision authz.Decision) {
	body := map[string]string{"error": decision.Reason}
	if decision.Code != "" {
		body["code"] = decision.Code
	}
	writeJSON(w, http.StatusForbidden, body)
}

// authorize enforces a route's declared permission across the whole platform,
// before the handler runs.
//
// Routes that address a single resource declare a scope type instead, and
// evaluate in the handler once they know which resource was addressed; see
// authorizeResource.
func (a *app) authorize(permission string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decision, err := a.authz.Evaluate(r.Context(), principalFrom(r.Context()), permission, authz.Global())
		if err != nil {
			logger.ErrorContext(r.Context(), "authorization evaluation failed", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization store unavailable"})
			return
		}
		if !decision.Allowed {
			writeDenied(w, decision)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorizeResource evaluates a route's permission against one resolved
// resource. It reports whether the caller may proceed, having already written
// the denial when they may not.
func (a *app) authorizeResource(w http.ResponseWriter, r *http.Request, permission, scopeType, scopeID string) bool {
	decision, err := a.authz.Evaluate(r.Context(), principalFrom(r.Context()), permission, authz.Resource(scopeType, scopeID))
	if err != nil {
		logger.ErrorContext(r.Context(), "authorization evaluation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization store unavailable"})
		return false
	}
	if !decision.Allowed {
		writeDenied(w, decision)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = time.Now().UTC().Format("20060102T150405.000000000Z07:00")
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDContextKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func fetchUserInfo(ctx context.Context, token string) (userInfo, error) {
	endpoint := os.Getenv("AUTHENTIK_USERINFO_URL")
	if endpoint == "" {
		return userInfo{}, errors.New("AUTHENTIK_USERINFO_URL is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return userInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return userInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return userInfo{}, errors.New("token rejected by authentik")
	}
	var info userInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return userInfo{}, err
	}
	if info.Sub == "" {
		return userInfo{}, errors.New("userinfo response has no subject")
	}
	return info, nil
}

func (a *app) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		info, err := fetchUserInfo(r.Context(), strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			logger.WarnContext(r.Context(), "authentication failed", "error", err.Error())
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired token"})
			return
		}
		username := info.PreferredUsername
		if username == "" {
			username = info.Email
		}
		globalUserID, created, err := a.db.EnsureIdentity(r.Context(), platformdb.Identity{
			Subject: info.Sub, Email: info.Email, DisplayName: info.Name, Username: username, Groups: unique(info.Groups),
		})
		if err != nil {
			logger.ErrorContext(r.Context(), "identity persistence failed", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "identity persistence unavailable"})
			return
		}
		if created {
			a.publish("identity.created", map[string]any{"global_user_id": globalUserID, "subject": info.Sub})
		}
		// Access is resolved once per request rather than per handler, so a
		// single request cannot observe two different authorization states.
		access, err := a.resolveAccess(r.Context(), info, globalUserID)
		if err != nil {
			logger.ErrorContext(r.Context(), "RBAC resolution failed", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "RBAC store unavailable"})
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, access)
		ctx = context.WithValue(ctx, principalContextKey, authz.Principal{
			ID: access.ID, Kind: authz.KindUser, Roles: access.Roles, Permissions: access.Permissions,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (a *app) resolveAccess(ctx context.Context, info userInfo, globalUserID string) (meResponse, error) {
	profile, err := a.db.ResolveAccess(ctx, unique(info.Groups), "human")
	if err != nil {
		return meResponse{}, err
	}
	username := info.PreferredUsername
	if username == "" {
		username = info.Email
	}
	return meResponse{
		ID:          globalUserID,
		Subject:     info.Sub,
		Email:       info.Email,
		Name:        info.Name,
		Username:    username,
		Groups:      unique(info.Groups),
		Roles:       profile.Roles,
		Permissions: profile.Permissions,
		Modules:     profile.Modules,
	}, nil
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey).(string)
	return id
}

func sourceIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
		return forwarded
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (a *app) audit(r *http.Request, access meResponse, action, resourceType, resourceID string, metadata map[string]any) {
	err := a.db.InsertAudit(r.Context(), platformdb.AuditEvent{Subject: access.Subject, GlobalUserID: access.ID, Action: action, ResourceType: resourceType, ResourceID: resourceID, RequestID: requestIDFrom(r.Context()), SourceIP: sourceIP(r), Metadata: metadata})
	if err != nil {
		logger.ErrorContext(r.Context(), "audit write failed", "error", err.Error())
	}
}

// publish sends a platform event, recording the outcome.
//
// The outcome is counted rather than only logged, because "no events" and
// "every event failed to publish" look identical on a dashboard that only
// counts successes.
func (a *app) publish(subject string, payload any) {
	if a.nc == nil {
		eventsPublished.Inc(subject, "unavailable")
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		eventsPublished.Inc(subject, "encode_failed")
		logger.Error("event could not be encoded", "subject", subject, "error", err.Error())
		return
	}
	if err := a.nc.Publish(subject, body); err != nil {
		eventsPublished.Inc(subject, "failed")
		logger.Error("event publication failed", "subject", subject, "error", err.Error())
		return
	}
	eventsPublished.Inc(subject, "published")
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"database": "ok", "nats": "ok"}
	status := "ok"
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.Ping(ctx); err != nil {
		checks["database"] = "error"
		status = "degraded"
	}
	if a.nc == nil || !a.nc.IsConnected() {
		checks["nats"] = "degraded"
		status = "degraded"
	}
	code := http.StatusOK
	if checks["database"] == "error" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, healthResponse{Status: status, Service: "bsystem-integration-core", Version: "0.5.0", Timestamp: time.Now().UTC().Format(time.RFC3339), Checks: checks})
}

func (a *app) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, accessFrom(r.Context()))
}

func (a *app) modules(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	allowed := map[string]bool{}
	for _, id := range access.Modules {
		allowed[id] = true
	}
	all, err := a.db.ListModules(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "module registry unavailable"})
		return
	}
	result := []platformdb.Module{}
	for _, item := range all {
		if allowed[item.ID] {
			result = append(result, item)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) createGlobalID(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	var input globalIDRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	input.EntityType = strings.TrimSpace(input.EntityType)
	input.Source = strings.TrimSpace(input.Source)
	input.SourceID = strings.TrimSpace(input.SourceID)
	if input.EntityType == "" || input.Source == "" || input.SourceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity_type, source and source_id are required"})
		return
	}
	entity, err := a.db.CreateGlobalEntity(r.Context(), input.EntityType, input.Source, input.SourceID, strings.TrimSpace(input.TenantID), input.Metadata)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.audit(r, access, "global_id.created", input.EntityType, entity.GlobalID, map[string]any{"source": input.Source, "source_id": input.SourceID})
	a.publish("global_id.created", entity)
	writeJSON(w, http.StatusCreated, entity)
}

func (a *app) resolveGlobalID(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Global ID is required"})
		return
	}
	entity, err := a.db.ResolveGlobalEntity(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Global ID not found"})
		return
	}
	a.audit(r, access, "global_id.read", entity.EntityType, entity.GlobalID, nil)
	writeJSON(w, http.StatusOK, entity)
}

func (a *app) auditEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := a.db.ListAudit(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := platformdb.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("database initialization failed: %v", err)
	}
	defer db.Close()

	var nc *nats.Conn
	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		nc, err = nats.Connect(natsURL, nats.Name("bsystem-integration-core"), nats.Timeout(5*time.Second), nats.MaxReconnects(-1))
		if err != nil {
			logger.Warn("NATS unavailable at startup", "error", err.Error())
			nc = nil
		} else {
			defer nc.Close()
		}
	}
	a := &app{db: db, nc: nc, authz: authz.New(db, authz.DefaultConfinedRoles()), searchProvider: searchProvider()}
	a.registerPlatformMetrics()

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	logger.Info("bsystem-integration-core listening", "version", "0.5.0", "addr", addr)
	log.Fatal(server.ListenAndServe())
}
