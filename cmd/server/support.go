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
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
	"github.com/ekucher/bsystem-integration-core/internal/support"
)

// supportScope is the scope a support record is authorized in.
//
// A record naming a customer is evaluated against that client, because that
// is where a grant is written: "this customer may see their own incidents" is
// a statement about the customer. A record with no customer falls back to
// itself, so a grant can still reach exactly one.
func supportScope(record support.Record) (string, string) {
	if record.ClientID != "" {
		return authz.ScopeClient, record.ClientID
	}
	return authz.ScopeResource, record.ID
}

// withSLA fills in the derived SLA state. It is computed on read rather than
// stored, because it is a function of the clock: a stored value would be
// wrong from the moment it was written.
func (a *app) withSLA(records []support.Record, policies map[string]support.Policy, now time.Time) {
	for i := range records {
		records[i].SLA = support.Evaluate(policies[records[i].Severity],
			records[i].CreatedAt, records[i].AcknowledgedAt, records[i].ResolvedAt, now)
	}
}

func (a *app) listSupportRecords(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, convErr := strconv.Atoi(strings.TrimSpace(query.Get("limit")))
	if convErr != nil {
		limit = 0
	}

	filters := platformdb.SupportQuery{
		ClientID: strings.TrimSpace(query.Get("client_id")),
		OpenOnly: strings.EqualFold(strings.TrimSpace(query.Get("open")), "true"),
		AfterID:  strings.TrimSpace(query.Get("cursor")),
		Limit:    notifications.ClampLimit(limit),
	}
	// Every filter is validated rather than passed through. An unknown value
	// would match nothing, and "no results" is indistinguishable from "none
	// exist" to whoever is reading.
	if value := strings.TrimSpace(query.Get("kind")); value != "" {
		kind, err := support.NormalizeKind(value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown kind", "code": "invalid_kind"})
			return
		}
		filters.Kind = kind
	}
	if value := strings.TrimSpace(query.Get("status")); value != "" {
		status, err := support.NormalizeStatus(value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown status", "code": "invalid_status"})
			return
		}
		filters.Status = status
	}
	if value := strings.TrimSpace(query.Get("severity")); value != "" {
		severity, err := support.NormalizeSeverity(value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown severity", "code": "invalid_severity"})
			return
		}
		filters.Severity = severity
	}

	records, total, err := a.db.ListSupportRecords(r.Context(), filters)
	if err != nil {
		logger.ErrorContext(r.Context(), "support listing failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "support store unavailable"})
		return
	}
	policies, err := a.db.SLAPolicies(r.Context())
	if err != nil {
		logger.ErrorContext(r.Context(), "SLA policy lookup failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "support store unavailable"})
		return
	}
	a.withSLA(records, policies, time.Now())

	next := ""
	if len(records) == filters.Limit && len(records) > 0 {
		next = records[len(records)-1].ID
	}
	writeJSON(w, http.StatusOK, collection[support.Record]{
		Data:       records,
		Pagination: paginationView{Total: total, Limit: len(records), NextCursor: next},
	})
}

// resolveSupportRecord loads a record and authorizes the caller for it,
// following the same order as every other detail endpoint: refuse a caller
// who cannot hold the permission anywhere before reading anything, so the
// endpoint cannot be used to enumerate which Global IDs name incidents.
func (a *app) resolveSupportRecord(w http.ResponseWriter, r *http.Request, permission string) (support.Record, bool) {
	principal := principalFrom(r.Context())
	confined := a.authz.IsConfined(principal)

	if !confined && !a.authorizeResource(w, r, permission, authz.ScopeGlobal, "*") {
		return support.Record{}, false
	}

	record, err := a.db.GetSupportRecord(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, platformdb.ErrSupportRecordNotFound):
		notFound(w)
		return support.Record{}, false
	case err != nil:
		logger.ErrorContext(r.Context(), "support lookup failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "support store unavailable"})
		return support.Record{}, false
	}

	scopeType, scopeID := supportScope(record)
	decision, err := a.authz.Evaluate(r.Context(), principal, permission, authz.Resource(scopeType, scopeID))
	if err != nil {
		logger.ErrorContext(r.Context(), "authorization evaluation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization store unavailable"})
		return support.Record{}, false
	}
	if !decision.Allowed {
		if confined {
			notFound(w)
		} else {
			writeDenied(w, decision)
		}
		return support.Record{}, false
	}
	return record, true
}

func (a *app) getSupportRecord(w http.ResponseWriter, r *http.Request) {
	record, ok := a.resolveSupportRecord(w, r, "support.incident.read")
	if !ok {
		return
	}
	policies, err := a.db.SLAPolicies(r.Context())
	if err != nil {
		logger.ErrorContext(r.Context(), "SLA policy lookup failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "support store unavailable"})
		return
	}
	records := []support.Record{record}
	a.withSLA(records, policies, time.Now())
	writeJSON(w, http.StatusOK, records[0])
}

// supportRequest is the create body.
type supportRequest struct {
	Kind      string             `json:"kind,omitempty"`
	Title     string             `json:"title"`
	Summary   string             `json:"summary,omitempty"`
	Severity  string             `json:"severity"`
	ClientID  string             `json:"client_id,omitempty"`
	Relations []support.Relation `json:"relations,omitempty"`
}

// supportUpdateRequest is the update body. Pointers distinguish a field the
// caller omitted from one they set to an empty value.
type supportUpdateRequest struct {
	Title     *string             `json:"title,omitempty"`
	Summary   *string             `json:"summary,omitempty"`
	Severity  *string             `json:"severity,omitempty"`
	Status    *string             `json:"status,omitempty"`
	Relations *[]support.Relation `json:"relations,omitempty"`
}

// verifyRelations checks that every relation names an entity of a type the
// platform knows and that actually resolves. A relation to something
// unidentifiable is a dangling pointer that reads as a fact.
func (a *app) verifyRelations(w http.ResponseWriter, r *http.Request, relations []support.Relation) ([]support.Relation, bool) {
	verified := make([]support.Relation, 0, len(relations))
	seen := map[string]bool{}
	for _, relation := range relations {
		entityType := strings.ToLower(strings.TrimSpace(relation.EntityType))
		globalID := strings.TrimSpace(relation.GlobalID)
		if !support.IsRelationType(entityType) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "relations may only name " + strings.Join(support.RelationTypes(), ", "),
				"code":  "invalid_relation",
			})
			return nil, false
		}
		entity, err := a.db.ResolveGlobalEntity(r.Context(), globalID)
		if err != nil || entity.EntityType != entityType {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "a relation does not name a known " + entityType,
				"code":  "invalid_relation",
			})
			return nil, false
		}
		key := entityType + "|" + globalID
		if seen[key] {
			continue
		}
		seen[key] = true
		verified = append(verified, support.Relation{EntityType: entityType, GlobalID: globalID})
	}
	return verified, true
}

func (a *app) createSupportRecord(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	var request supportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	request.Title = strings.TrimSpace(request.Title)
	if request.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}
	kind, err := support.NormalizeKind(request.Kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown kind", "code": "invalid_kind"})
		return
	}
	severity, err := support.NormalizeSeverity(request.Severity)
	if err != nil {
		// There is no default severity: an unstated one is a question nobody
		// has answered, and answering it here would pick the response time on
		// the reporter's behalf.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "severity must be one of low, medium, high, critical", "code": "invalid_severity",
		})
		return
	}

	clientID, ok := a.resolveRelation(w, r, request.ClientID, "client", "client_id")
	if !ok {
		return
	}
	relations, ok := a.verifyRelations(w, r, request.Relations)
	if !ok {
		return
	}

	entity, err := a.db.CreateGlobalEntity(r.Context(), "incident", "bsystem", newSupportSourceID(), clientID, map[string]any{"managed_by": "support"})
	if err != nil {
		logger.ErrorContext(r.Context(), "support Global ID allocation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "support store unavailable"})
		return
	}

	record, err := a.db.CreateSupportRecord(r.Context(), support.Record{
		ID: entity.GlobalID, Kind: kind, Title: request.Title,
		Summary: strings.TrimSpace(request.Summary), Severity: severity,
		Status: support.StatusNew, ClientID: clientID,
		ReportedBy: access.ID, Relations: relations,
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "support record creation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "support store unavailable"})
		return
	}
	a.audit(r, access, "incident.created", "incident", record.ID, map[string]any{"kind": record.Kind, "severity": record.Severity})
	a.publishSupportEvent(r, record, "incident.created")

	policies, _ := a.db.SLAPolicies(r.Context())
	records := []support.Record{record}
	a.withSLA(records, policies, time.Now())
	writeJSON(w, http.StatusCreated, records[0])
}

func (a *app) updateSupportRecord(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	existing, ok := a.resolveSupportRecord(w, r, "support.incident.write")
	if !ok {
		return
	}

	var request supportUpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	update := platformdb.SupportUpdate{}
	if request.Title != nil {
		title := strings.TrimSpace(*request.Title)
		if title == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title cannot be cleared"})
			return
		}
		update.Title = &title
	}
	if request.Summary != nil {
		summary := strings.TrimSpace(*request.Summary)
		update.Summary = &summary
	}
	if request.Severity != nil {
		severity, err := support.NormalizeSeverity(*request.Severity)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown severity", "code": "invalid_severity"})
			return
		}
		update.Severity = &severity
	}

	newStatus := existing.Status
	if request.Status != nil {
		status, err := support.NormalizeStatus(*request.Status)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown status", "code": "invalid_status"})
			return
		}
		if err := support.CanTransition(existing.Status, status); err != nil {
			// The refusal names both ends, because a caller who sent a status
			// needs to know which move was refused rather than being told the
			// value was wrong when it was not.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": existing.Status + " cannot become " + status,
				"code":  "invalid_transition",
			})
			return
		}
		update.Status = &status
		newStatus = status
		// The SLA moments are recorded when the status first reaches them and
		// never moved again, so reopening does not erase that the record was
		// once resolved on time.
		update.Acknowledge = status != support.StatusNew
		update.Resolve = status == support.StatusResolved || status == support.StatusClosed
	}
	if request.Relations != nil {
		relations, ok := a.verifyRelations(w, r, *request.Relations)
		if !ok {
			return
		}
		update.Relations = relations
	}

	record, err := a.db.UpdateSupportRecord(r.Context(), existing.ID, update)
	switch {
	case errors.Is(err, platformdb.ErrSupportRecordNotFound):
		notFound(w)
		return
	case err != nil:
		logger.ErrorContext(r.Context(), "support record update failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "support store unavailable"})
		return
	}

	a.audit(r, access, "incident.updated", "incident", record.ID, map[string]any{"status": record.Status, "severity": record.Severity})
	event := "incident.updated"
	if newStatus == support.StatusResolved && existing.Status != support.StatusResolved {
		event = "incident.resolved"
	}
	a.publishSupportEvent(r, record, event)

	policies, _ := a.db.SLAPolicies(r.Context())
	records := []support.Record{record}
	a.withSLA(records, policies, time.Now())
	writeJSON(w, http.StatusOK, records[0])
}

// publishSupportEvent puts a support change on the bus and, where people are
// interrupted by it, raises a notification.
//
// The title travels; the summary does not. A title is a one-line label
// somebody wrote to be read at a glance, while a summary is where a reporter
// pastes logs, addresses and occasionally a credential — and an event is
// delivered far more widely than the record it came from.
func (a *app) publishSupportEvent(r *http.Request, record support.Record, event string) {
	a.publish(events.Subject(event), map[string]any{
		"event": event, "source": "bsystem-support", "severity": eventSeverityFor(record.Severity),
		"entity_id": record.ID, "tenant_id": record.ClientID,
		"occurred_at": record.UpdatedAt, "request_id": requestIDFrom(r.Context()),
		"data": map[string]any{"message": record.Title, "status": record.Status, "kind": record.Kind},
	})
	a.notifyFromEvent(r, notificationSourceEvent{
		Event: event, Source: "bsystem-support", Severity: eventSeverityFor(record.Severity),
		EntityID: record.ID, TenantID: record.ClientID,
		RequestID: requestIDFrom(r.Context()), OccurredAt: record.UpdatedAt,
		Body: record.Title,
	})
}

// eventSeverityFor maps an incident severity onto the platform's event
// severity. They are different scales — one is a commitment about response,
// the other a description of what happened — so the mapping is explicit
// rather than a shared constant.
func eventSeverityFor(severity string) string {
	switch severity {
	case support.SeverityCritical:
		return "critical"
	case support.SeverityHigh:
		return "error"
	case support.SeverityMedium:
		return "warning"
	default:
		return "info"
	}
}

// newSupportSourceID returns the source identifier a platform-originated
// record is keyed by. Support records are raised in BSYSTEM rather than
// mirrored from an upstream, so the platform supplies the key itself.
func newSupportSourceID() string {
	return "support-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
}
