package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/notifications"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// notificationCollection is the list envelope. It carries the counts a
// notification centre needs — an unread badge otherwise has to page through
// the whole history to draw a number.
type notificationCollection struct {
	Data        []notifications.Notification `json:"data"`
	Pagination  paginationView               `json:"pagination"`
	UnreadCount int                          `json:"unread_count"`
}

// notificationQueryFor derives what the caller may read from the principal the
// request was authenticated as.
//
// A scope-confined principal sees only notifications addressed to it by name.
// Its role permissions describe what it may do inside scopes it has been
// granted, so treating them as an audience would hand a customer every
// platform-wide notification their role's permissions happen to match — the
// tenant boundary leaking through a side channel. An undecided audience
// denies, as everywhere else.
func (a *app) notificationQueryFor(access meResponse) platformdb.NotificationQuery {
	principal := authz.Principal{ID: access.ID, Kind: authz.KindUser, Roles: access.Roles, Permissions: access.Permissions}
	query := platformdb.NotificationQuery{GlobalUserID: access.ID}
	switch {
	case a.authz.IsConfined(principal):
		// Direct addressing only.
	case hasString(access.Permissions, authz.PermissionAll):
		query.AllAudiences = true
	default:
		query.AudiencePermissions = access.Permissions
	}
	return query
}

func (a *app) listNotifications(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	after, err := notifications.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pagination cursor", "code": "invalid_cursor"})
		return
	}
	limit, convErr := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if convErr != nil {
		limit = 0
	}

	query := a.notificationQueryFor(access)
	query.AfterID = after
	query.Limit = notifications.ClampLimit(limit)
	query.UnreadOnly = strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("unread")), "true")

	items, total, unread, err := a.db.ListNotifications(r.Context(), query)
	if err != nil {
		logger.ErrorContext(r.Context(), "notification listing failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notification store unavailable"})
		return
	}

	// A next cursor is issued only on a full page. A short page ends the
	// collection; issuing one anyway would make a caller loop on an empty
	// result.
	next := ""
	if len(items) == query.Limit && len(items) > 0 {
		next = notifications.EncodeCursor(items[len(items)-1].ID)
	}
	writeJSON(w, http.StatusOK, notificationCollection{
		Data:        items,
		Pagination:  paginationView{Total: total, Limit: len(items), NextCursor: next},
		UnreadCount: unread,
	})
}

func (a *app) markNotificationRead(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		// A malformed id is answered as not found rather than as a bad
		// request, so probing with ids tells a caller nothing they did not
		// already know.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notification not found", "code": "not_found"})
		return
	}
	err = a.db.MarkNotificationRead(r.Context(), id, a.notificationQueryFor(access))
	switch {
	case errors.Is(err, platformdb.ErrNotificationNotVisible):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notification not found", "code": "not_found"})
		return
	case err != nil:
		logger.ErrorContext(r.Context(), "notification read marking failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notification store unavailable"})
		return
	}
	a.audit(r, access, "notification.read", "notification", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(http.StatusNoContent)
}

// notificationRequest is the machine API's body for raising a notification
// directly, for a publisher that knows its recipient.
type notificationRequest struct {
	RecipientID        string `json:"recipient_id,omitempty"`
	AudiencePermission string `json:"audience_permission,omitempty"`
	Severity           string `json:"severity"`
	Title              string `json:"title"`
	Body               string `json:"body,omitempty"`
	EntityID           string `json:"entity_id,omitempty"`
	TenantID           string `json:"tenant_id,omitempty"`
	Source             string `json:"source"`
	Event              string `json:"event,omitempty"`
}

func (a *app) servicePublishNotification(w http.ResponseWriter, r *http.Request) {
	var request notificationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	request.RecipientID = strings.TrimSpace(request.RecipientID)
	request.AudiencePermission = strings.TrimSpace(request.AudiencePermission)
	request.Title = strings.TrimSpace(request.Title)
	request.Source = strings.TrimSpace(request.Source)
	request.Event = strings.TrimSpace(request.Event)

	if (request.RecipientID == "") == (request.AudiencePermission == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "exactly one of recipient_id and audience_permission is required"})
		return
	}
	if request.Title == "" || request.Source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title and source are required"})
		return
	}
	severity := notifications.EscalateTo("info", request.Severity)

	// An address that cannot be resolved is refused rather than stored. A
	// notification nobody can read is not a notification, and accepting one
	// would report success for a message that will never be delivered.
	if request.RecipientID != "" {
		known, err := a.db.IdentityExists(r.Context(), request.RecipientID)
		if err != nil {
			logger.ErrorContext(r.Context(), "recipient lookup failed", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notification store unavailable"})
			return
		}
		if !known {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recipient_id is not a known Global user ID"})
			return
		}
	} else {
		known, err := a.db.PermissionExists(r.Context(), request.AudiencePermission)
		if err != nil {
			logger.ErrorContext(r.Context(), "audience lookup failed", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notification store unavailable"})
			return
		}
		if !known {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audience_permission is not a known permission"})
			return
		}
	}

	now := time.Now().UTC()
	notification := notifications.Notification{
		Event:              request.Event,
		Source:             request.Source,
		Severity:           severity,
		Title:              request.Title,
		Body:               request.Body,
		DeepLink:           notifications.DeepLink(request.EntityID),
		EntityID:           strings.TrimSpace(request.EntityID),
		TenantID:           strings.TrimSpace(request.TenantID),
		RecipientID:        request.RecipientID,
		AudiencePermission: request.AudiencePermission,
		CorrelationID:      requestIDFrom(r.Context()),
		OccurredAt:         now.Format(time.RFC3339),
	}
	if notification.Event == "" {
		notification.Event = "notification.raised"
	}
	id, err := a.db.InsertNotification(r.Context(), notification, now)
	if err != nil {
		logger.ErrorContext(r.Context(), "notification write failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notification store unavailable"})
		return
	}
	notification.ID = id
	writeJSON(w, http.StatusCreated, notification)
}

// notifyFromEvent raises the notification a mapped event calls for.
//
// It is best effort on purpose: an event has already been accepted and
// published by the time this runs, and failing the publish because a
// notification could not be stored would lose the event as well. The failure
// is logged and counted instead.
func (a *app) notifyFromEvent(r *http.Request, envelope notificationSourceEvent) {
	notification, mapped := notifications.FromEvent(notifications.Event{
		Event:         envelope.Event,
		Source:        envelope.Source,
		Severity:      envelope.Severity,
		EntityID:      envelope.EntityID,
		TenantID:      envelope.TenantID,
		CorrelationID: envelope.RequestID,
		OccurredAt:    envelope.OccurredAt.UTC().Format(time.RFC3339),
		Body:          envelope.Body,
	})
	if !mapped {
		return
	}
	if _, err := a.db.InsertNotification(r.Context(), notification, envelope.OccurredAt); err != nil {
		notificationsRaised.Inc(notification.Event, "failed")
		logger.ErrorContext(r.Context(), "notification could not be raised", "event", notification.Event, "error", err.Error())
		return
	}
	notificationsRaised.Inc(notification.Event, "raised")
}

// notificationSourceEvent is the part of an event envelope a notification is
// derived from, flattened so the mapping does not depend on the event
// package's representation of the payload.
type notificationSourceEvent struct {
	Event      string
	Source     string
	Severity   string
	EntityID   string
	TenantID   string
	RequestID  string
	OccurredAt time.Time
	Body       string
}

// eventBody extracts the human-readable detail a publisher supplied.
//
// Only a "message" string is taken. Copying the whole payload into a
// notification body would put arbitrary upstream data — potentially
// confidential — in front of everyone who holds the audience permission.
func eventBody(data map[string]any) string {
	message, _ := data["message"].(string)
	return strings.TrimSpace(message)
}
