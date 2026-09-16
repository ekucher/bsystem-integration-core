// Package notifications turns platform events into notifications and decides
// who a notification is addressed to.
//
// Addressing is the part worth stating plainly. BSYSTEM has no authoritative
// mapping from an operational event to a person: nobody has told the platform
// who owns a build, who is on call for a backup, or which humans want to hear
// about an incident. Inventing one would produce notifications addressed to
// people who never asked for them and, worse, would be a second authorization
// system disagreeing with the first.
//
// So a mapped event is addressed to a permission rather than to a person. The
// notification says "this concerns whoever may read server state", and who
// satisfies that is resolved from authentik and RBAC at read time — the same
// answer the rest of the platform would give. A publisher that does know a
// recipient may name one explicitly instead.
package notifications

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Addressing modes. Exactly one is set on any notification.
const (
	// AudiencePermission addresses everyone who may exercise a permission.
	// The set is resolved when a notification is read, not when it is raised,
	// so revoking access removes the notification from view.
	AudienceMode = "audience"
	// RecipientMode addresses one named Global user ID.
	RecipientMode = "recipient"
)

// Severities, ordered. This mirrors events.Envelope's domain.
var severityOrder = map[string]int{
	"debug": 0, "info": 1, "warning": 2, "error": 3, "critical": 4,
}

// Mapping is the platform's classification of one event.
type Mapping struct {
	// Permission is the audience. It must be a permission that exists in
	// RBAC; a notification addressed to a permission nobody holds is
	// invisible rather than broadcast, which is the safe direction.
	Permission string
	// Severity is the platform's own classification, used as a floor. A
	// publisher may escalate an event it knows is worse than usual, but it
	// cannot quietly downgrade one the platform considers critical.
	Severity string
	// Title is the human summary. Detail belongs in the body, which comes
	// from the event, because the platform does not know it.
	Title string
}

// mappings is the authoritative event-to-notification table.
//
// An event that is not here raises no notification at all. That is deliberate:
// every entry is a decision that some group of people wants to be interrupted,
// and the default for an unknown event is not to interrupt anyone.
var mappings = map[string]Mapping{
	"backup.failed":  {Permission: "operations.server.read", Severity: "critical", Title: "Backup failed"},
	"server.offline": {Permission: "operations.server.read", Severity: "critical", Title: "Server offline"},
	"build.failed":   {Permission: "development.repo.read", Severity: "error", Title: "Build failed"},
	// qa.testcase.read rather than qa.report.read: the QA role holds the
	// former and not the latter, and a failed test run has to reach the
	// people who run tests. The E2E suite caught this addressed to a
	// permission only Managers hold.
	"test.failed":      {Permission: "qa.testcase.read", Severity: "error", Title: "Test run failed"},
	"incident.created": {Permission: "support.incident.read", Severity: "error", Title: "Incident created"},
	"release.created":  {Permission: "development.repo.read", Severity: "info", Title: "Release created"},
}

// Mappings returns the table, for documentation and contract tests.
func Mappings() map[string]Mapping {
	copied := make(map[string]Mapping, len(mappings))
	for event, mapping := range mappings {
		copied[event] = mapping
	}
	return copied
}

// Notification is a platform notification as it is stored and served.
type Notification struct {
	ID       int64  `json:"id"`
	Event    string `json:"event"`
	Source   string `json:"source"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	// DeepLink is a HUB path, or empty when the platform has no page for the
	// entity. An empty link is better than one that leads nowhere.
	DeepLink string `json:"deep_link,omitempty"`
	EntityID string `json:"entity_id,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`
	// RecipientID and AudiencePermission are the addressing; exactly one is
	// set. Both are returned so a reader can tell whether a notification was
	// meant for them personally or for their role.
	RecipientID        string `json:"recipient_id,omitempty"`
	AudiencePermission string `json:"audience_permission,omitempty"`
	CorrelationID      string `json:"correlation_id,omitempty"`
	OccurredAt         string `json:"occurred_at"`
	Read               bool   `json:"read"`
}

// Event is the subset of an event envelope a notification is built from.
type Event struct {
	Event         string
	Source        string
	Severity      string
	EntityID      string
	TenantID      string
	CorrelationID string
	OccurredAt    string
	Body          string
}

// FromEvent builds the notification a mapped event raises. The second result
// is false for an event the platform does not notify on, which is most of
// them.
func FromEvent(e Event) (Notification, bool) {
	mapping, mapped := mappings[strings.TrimSpace(e.Event)]
	if !mapped {
		return Notification{}, false
	}
	return Notification{
		Event:              e.Event,
		Source:             e.Source,
		Severity:           EscalateTo(mapping.Severity, e.Severity),
		Title:              mapping.Title,
		Body:               e.Body,
		DeepLink:           DeepLink(e.EntityID),
		EntityID:           e.EntityID,
		TenantID:           e.TenantID,
		AudiencePermission: mapping.Permission,
		CorrelationID:      e.CorrelationID,
		OccurredAt:         e.OccurredAt,
	}, true
}

// EscalateTo returns the more severe of the platform's classification and the
// publisher's, so a publisher can raise an event's severity but not lower it.
// An unrecognised publisher severity is ignored rather than trusted.
func EscalateTo(floor, reported string) string {
	reported = strings.ToLower(strings.TrimSpace(reported))
	base, known := severityOrder[floor]
	if !known {
		return reported
	}
	if rank, ok := severityOrder[reported]; ok && rank > base {
		return reported
	}
	return floor
}

// deepLinkRoutes maps a Global ID prefix to the HUB route that shows it.
//
// Only entity types the HUB actually has a page for appear here. Operational
// and support entities (SRV, INC, BUG, TST, REL) are deliberately absent
// until those areas of the HUB exist: a link to a page that does not exist is
// worse than no link, because a user follows it.
var deepLinkRoutes = map[string]string{
	"CL":  "/clients/",
	"CT":  "/contacts/",
	"PR":  "/projects/",
	"TSK": "/issues/",
	"DOC": "/documents/",
}

// DeepLink returns the HUB path for a Global ID, or empty when there is none.
func DeepLink(globalID string) string {
	globalID = strings.TrimSpace(globalID)
	prefix, _, found := strings.Cut(globalID, "-")
	if !found || prefix == "" {
		return ""
	}
	route, known := deepLinkRoutes[prefix]
	if !known {
		return ""
	}
	return route + globalID
}

// Cursors.
//
// Notifications page by the id of the last row returned rather than by an
// offset. A notification store is append-heavy at the head, and an offset
// cursor would make a new arrival shift every later page — a caller walking
// the collection would see the same notification twice and miss another.
// Keyset paging is stable under insertion because it names a position rather
// than a distance.
const cursorPrefix = "n:"

// ErrInvalidCursor means the caller supplied a cursor this platform did not
// issue.
var ErrInvalidCursor = errors.New("invalid pagination cursor")

// EncodeCursor returns the opaque cursor naming a position in the collection.
func EncodeCursor(lastID int64) string {
	if lastID <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(lastID, 10)))
}

// DecodeCursor returns the id a cursor names. An empty cursor starts at the
// newest notification.
func DecodeCursor(cursor string) (int64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrInvalidCursor
	}
	body, found := strings.CutPrefix(string(decoded), cursorPrefix)
	if !found {
		return 0, ErrInvalidCursor
	}
	id, err := strconv.ParseInt(body, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidCursor
	}
	return id, nil
}

// Page bounds. The cap is what stops one request pulling the whole history.
const (
	DefaultLimit = 25
	MaxLimit     = 100
)

// ClampLimit bounds a requested page size, matching the collection
// convention used elsewhere in the API: too large is clamped, not refused.
func ClampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}
