package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/events"
	"github.com/ekucher/bsystem-integration-core/internal/notifications"
	"github.com/ekucher/bsystem-integration-core/internal/operations"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// serverScope is the scope a server is authorized in.
//
// A server that belongs to a customer is evaluated against that client,
// because that is where a grant would naturally be written: "this customer
// may see their own infrastructure" is a statement about the customer, not
// about each machine. A server with no owner falls back to itself, so a grant
// can still be written for exactly one host.
func serverScope(server operations.Server) (string, string) {
	if server.ClientID != "" {
		return authz.ScopeClient, server.ClientID
	}
	return authz.ScopeResource, server.ID
}

func (a *app) listServers(w http.ResponseWriter, r *http.Request) {
	limit, convErr := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if convErr != nil {
		limit = 0
	}
	query := platformdb.ServerQuery{
		Status:      strings.TrimSpace(r.URL.Query().Get("status")),
		Environment: strings.TrimSpace(r.URL.Query().Get("environment")),
		AfterID:     strings.TrimSpace(r.URL.Query().Get("cursor")),
		Limit:       notifications.ClampLimit(limit),
	}
	// The cursor is the last Global ID returned. It is not opaque here and
	// deliberately so: it is already a public, immutable identifier the
	// caller has just been given, and encoding it would suggest a position
	// that can drift when it cannot.
	servers, total, err := a.db.ListServers(r.Context(), query)
	if err != nil {
		logger.ErrorContext(r.Context(), "server listing failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "operations store unavailable"})
		return
	}
	next := ""
	if len(servers) == query.Limit && len(servers) > 0 {
		next = servers[len(servers)-1].ID
	}
	writeJSON(w, http.StatusOK, collection[operations.Server]{
		Data:       servers,
		Pagination: paginationView{Total: total, Limit: len(servers), NextCursor: next},
	})
}

func (a *app) getServer(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r.Context())
	confined := a.authz.IsConfined(principal)

	// A caller who cannot hold the permission anywhere is refused before the
	// lookup happens. Looking up first would let them tell an existing server
	// from a missing one by the status code, and enumerate which Global IDs
	// name infrastructure. A scope-confined caller is exempt, because its
	// authority is per-resource and has to be evaluated against the resolved
	// server instead.
	if !confined && !a.authorizeResource(w, r, "operations.server.read", authz.ScopeGlobal, "*") {
		return
	}

	server, err := a.db.GetServer(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, platformdb.ErrServerNotFound):
		notFound(w)
		return
	case err != nil:
		logger.ErrorContext(r.Context(), "server lookup failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "operations store unavailable"})
		return
	}

	scopeType, scopeID := serverScope(server)
	decision, err := a.authz.Evaluate(r.Context(), principal, "operations.server.read", authz.Resource(scopeType, scopeID))
	if err != nil {
		logger.ErrorContext(r.Context(), "authorization evaluation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization store unavailable"})
		return
	}
	if !decision.Allowed {
		// A confined caller is told the server does not exist, because
		// "exists but not yours" is what it would use to enumerate its
		// neighbours' infrastructure.
		if confined {
			notFound(w)
		} else {
			writeDenied(w, decision)
		}
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (a *app) listOperationsEvents(w http.ResponseWriter, r *http.Request) {
	limit, convErr := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if convErr != nil {
		limit = 0
	}
	after := int64(0)
	if cursor := strings.TrimSpace(r.URL.Query().Get("cursor")); cursor != "" {
		parsed, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || parsed <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pagination cursor", "code": "invalid_cursor"})
			return
		}
		after = parsed
	}
	event := strings.TrimSpace(r.URL.Query().Get("event"))
	if event != "" {
		if _, err := operations.Lookup(event); err != nil {
			// An unknown event name is refused rather than ignored: a filter
			// that quietly matched nothing and a filter that quietly matched
			// everything are both read as "there is nothing".
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown operations event", "code": "invalid_event"})
			return
		}
	}

	reports, total, err := a.db.ListOperationsEvents(r.Context(), platformdb.OperationsEventQuery{
		ServerID: strings.TrimSpace(r.URL.Query().Get("server_id")),
		Event:    event,
		AfterID:  after,
		Limit:    notifications.ClampLimit(limit),
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "operations event listing failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "operations store unavailable"})
		return
	}
	next := ""
	if len(reports) > 0 && len(reports) == notifications.ClampLimit(limit) {
		next = strconv.FormatInt(reports[len(reports)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, collection[operations.Report]{
		Data:       reports,
		Pagination: paginationView{Total: total, Limit: len(reports), NextCursor: next},
	})
}

// serverRequest registers or updates a server from its reporter.
type serverRequest struct {
	Source      string `json:"source"`
	SourceID    string `json:"source_id"`
	Name        string `json:"name"`
	Environment string `json:"environment,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
}

func (a *app) serviceRegisterServer(w http.ResponseWriter, r *http.Request) {
	var request serverRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	request.Source = strings.TrimSpace(request.Source)
	request.SourceID = strings.TrimSpace(request.SourceID)
	request.Name = strings.TrimSpace(request.Name)
	if request.Source == "" || request.SourceID == "" || request.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source, source_id and name are required"})
		return
	}
	environment, err := operations.NormalizeEnvironment(request.Environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_environment"})
		return
	}

	// Relations are verified before anything is stored. An operations record
	// attached to the wrong customer is worse than one attached to none, and
	// an unresolvable Global ID is exactly the ambiguous ownership the
	// platform is required to refuse rather than guess at.
	clientID, ok := a.resolveRelation(w, r, request.ClientID, "client", "client_id")
	if !ok {
		return
	}
	projectID, ok := a.resolveRelation(w, r, request.ProjectID, "project", "project_id")
	if !ok {
		return
	}

	// The Global ID comes from the platform's own allocator, keyed by the
	// reporter's identifier, so re-registering the same host returns the same
	// SRV-* rather than minting another.
	entity, err := a.db.CreateGlobalEntity(r.Context(), "server", request.Source, request.SourceID, clientID, map[string]any{"managed_by": "operations"})
	if err != nil {
		logger.ErrorContext(r.Context(), "server Global ID allocation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "operations store unavailable"})
		return
	}

	server, err := a.db.UpsertServer(r.Context(), operations.Server{
		ID: entity.GlobalID, Name: request.Name, Environment: environment,
		ClientID: clientID, ProjectID: projectID,
		Source: request.Source, SourceID: request.SourceID,
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "server registration failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "operations store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, server)
}

// resolveRelation checks that an optional Global ID names an entity of the
// expected type. It reports whether the caller may proceed, having already
// written the refusal when they may not.
func (a *app) resolveRelation(w http.ResponseWriter, r *http.Request, globalID, entityType, field string) (string, bool) {
	globalID = strings.TrimSpace(globalID)
	if globalID == "" {
		return "", true
	}
	entity, err := a.db.ResolveGlobalEntity(r.Context(), globalID)
	if err != nil || entity.EntityType != entityType {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": field + " does not name a known " + entityType,
			"code":  "invalid_relation",
		})
		return "", false
	}
	return globalID, true
}

// operationsEventRequest is one report about a server.
type operationsEventRequest struct {
	ServerID   string    `json:"server_id"`
	Event      string    `json:"event"`
	Severity   string    `json:"severity,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	OccurredAt time.Time `json:"occurred_at,omitempty"`
	Source     string    `json:"source"`
}

func (a *app) serviceReportOperationsEvent(w http.ResponseWriter, r *http.Request) {
	var request operationsEventRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	request.ServerID = strings.TrimSpace(request.ServerID)
	request.Event = strings.TrimSpace(request.Event)
	request.Source = strings.TrimSpace(request.Source)
	if request.ServerID == "" || request.Source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server_id and source are required"})
		return
	}

	classification, err := operations.Lookup(request.Event)
	if err != nil {
		// The vocabulary is closed on purpose. An operations feed that
		// accepts any name becomes a log, and nobody can write a query or a
		// notification rule against a log whose contents are unbounded.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown operations event", "code": "invalid_event"})
		return
	}

	server, err := a.db.GetServer(r.Context(), request.ServerID)
	switch {
	case errors.Is(err, platformdb.ErrServerNotFound):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server_id does not name a registered server", "code": "invalid_relation"})
		return
	case err != nil:
		logger.ErrorContext(r.Context(), "server lookup failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "operations store unavailable"})
		return
	}

	occurredAt := request.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	report, err := a.db.InsertOperationsEvent(r.Context(), operations.Report{
		ServerID:      server.ID,
		Event:         request.Event,
		Severity:      operations.EscalateTo(classification.Severity, request.Severity),
		Summary:       strings.TrimSpace(request.Summary),
		Source:        request.Source,
		CorrelationID: requestIDFrom(r.Context()),
		OccurredAt:    occurredAt,
	}, classification.Status)
	if err != nil {
		logger.ErrorContext(r.Context(), "operations event write failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "operations store unavailable"})
		return
	}
	operationsReported.Inc(report.Event, report.Severity)

	// The report becomes a platform event and, where the event is one people
	// are interrupted by, a notification. Both are best effort and neither
	// fails the request: the report is already stored, and losing it to make
	// a secondary effect look atomic would be the worse trade.
	a.publish(events.Subject(report.Event), map[string]any{
		"event": report.Event, "source": report.Source, "severity": report.Severity,
		"entity_id": report.ServerID, "tenant_id": server.ClientID,
		"occurred_at": report.OccurredAt, "request_id": report.CorrelationID,
		"data": map[string]any{"message": report.Summary},
	})
	a.notifyFromEvent(r, notificationSourceEvent{
		Event: report.Event, Source: report.Source, Severity: report.Severity,
		EntityID: report.ServerID, TenantID: server.ClientID,
		RequestID: report.CorrelationID, OccurredAt: report.OccurredAt,
		Body: report.Summary,
	})

	writeJSON(w, http.StatusAccepted, report)
}

// notificationMappingFor reports whether an event raises a notification, and
// to whom. It exists so the operations vocabulary and the notification table
// can be checked against each other; they are maintained separately, and
// nothing else would notice them drifting apart.
func notificationMappingFor(event string) (notifications.Mapping, bool) {
	mapping, mapped := notifications.Mappings()[event]
	return mapping, mapped
}
