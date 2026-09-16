// Package search defines the platform's normalized search surface.
//
// Search is the endpoint most likely to disclose something by accident. Every
// other read starts from an identifier the caller already had; a search
// returns records the caller has never named, drawn from every module at
// once. So the shape of a searchable document is built around the question
// "who may see this", and the answer travels with the document rather than
// being reconstructed at query time from whatever the result happens to
// contain.
//
// The package holds no provider implementation of its own beyond the
// in-memory one used for tests and the E2E stack. A real engine plugs in
// through Provider.
package search

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Entity types a document may have. They match the Global ID families the
// platform allocates, so a caller filtering by type is filtering by the same
// vocabulary the rest of the API uses.
const (
	TypeClient   = "client"
	TypeContact  = "contact"
	TypeProject  = "project"
	TypeIssue    = "issue"
	TypeDocument = "document"
)

// Types returns every indexable entity type.
func Types() []string {
	return []string{TypeClient, TypeContact, TypeProject, TypeIssue, TypeDocument}
}

// Document is one searchable record.
//
// It carries no upstream payload. A search index is read by more people than
// any single endpoint, and copying a source record into it would put upstream
// fields — some confidential — in front of everyone who can search. Title and
// Summary are the only text, and both are expected to be safe to show to
// anyone who passes the document's own authorization.
type Document struct {
	// ID is the platform Global ID. It is the identity of the document: the
	// index is keyed by it, so re-indexing the same entity replaces rather
	// than duplicates.
	ID   string `json:"id"`
	Type string `json:"type"`
	// Title and Summary are the only searchable text.
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	// Source is the authoritative system the record lives in.
	Source string `json:"source"`
	// TenantID is the owning customer, when the entity has one.
	TenantID string `json:"tenant_id,omitempty"`
	// Permissions are the permissions that grant read access, any one of
	// which is enough.
	//
	// An empty list means nobody may see the document. That is the
	// deliberate direction for the default: a document indexed without an
	// answer to "who may read this" is one the platform cannot show safely,
	// and making it invisible is the failure a reader notices rather than the
	// one they do not.
	Permissions []string `json:"permissions"`
	// ScopeType and ScopeID place the document for a scope-confined
	// principal, whose authority is per-resource rather than platform-wide.
	// They must name the scope a grant would be written against — a client
	// document is scoped to its client, an issue to the resource itself.
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	// UpdatedAt is when the source record last changed, used for ordering.
	UpdatedAt time.Time `json:"updated_at"`
}

// Hit is a document returned by a search, with the score that ranked it.
type Hit struct {
	Document
	Score float64 `json:"score"`
}

// Query is a search request as the platform understands it.
type Query struct {
	// Text is the caller's query. An empty query matches nothing rather than
	// everything: a search endpoint that returns the whole index when asked
	// for nothing is an export endpoint wearing a search endpoint's name.
	Text string
	// Types restricts the search. Empty means every type.
	Types []string
	// Permissions are the permissions the caller holds, used to narrow the
	// candidate set before the authorization decision is made. It is an
	// optimisation and a second line of defence, never the decision itself.
	Permissions []string
	// AllPermissions is set for an administrator and skips that narrowing.
	AllPermissions bool
	// Offset and Limit bound the window.
	Offset int
	Limit  int
}

// Page is a window of results.
type Page struct {
	Hits []Hit
	// Total is the number of candidates the provider matched before the
	// platform applied its authorization filter. It is never returned to a
	// caller: it counts records they may not be allowed to know exist.
	Total int
	// Examined is how many candidates the provider returned, which is what
	// the next cursor advances past.
	Examined int
}

// Provider is a search engine the platform can query.
//
// Index is an upsert keyed by Document.ID, so replaying an index request is
// safe. Delete is likewise idempotent: removing an absent document succeeds,
// because a delete that fails when the record is already gone turns a retry
// into an error.
type Provider interface {
	// Name identifies the provider in health output and errors.
	Name() string
	Index(ctx context.Context, documents ...Document) error
	Delete(ctx context.Context, ids ...string) error
	Search(ctx context.Context, query Query) (Page, error)
}

// Errors the platform distinguishes.
var (
	// ErrInvalidDocument means a document cannot be indexed as given.
	ErrInvalidDocument = errors.New("invalid search document")
	// ErrInvalidCursor means the caller supplied a cursor this platform did
	// not issue.
	ErrInvalidCursor = errors.New("invalid pagination cursor")
	// ErrUnavailable means the search engine could not be reached. It is
	// deliberately distinct from "no results": telling a user their search
	// found nothing when the engine was down is a lie they will act on.
	ErrUnavailable = errors.New("search provider unavailable")
)

// Page bounds.
const (
	DefaultLimit = 20
	MaxLimit     = 50
	// MinQueryLength stops a single character from matching most of the
	// index. It is a relevance bound rather than a security one — the
	// authorization filter applies whatever the query is.
	MinQueryLength = 2
)

// ClampLimit bounds a requested page size, matching the rest of the API:
// too large is clamped rather than refused.
func ClampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// Normalize validates and tidies a document before it is indexed.
func (d *Document) Normalize(now time.Time) error {
	d.ID = strings.TrimSpace(d.ID)
	d.Type = strings.ToLower(strings.TrimSpace(d.Type))
	d.Title = strings.TrimSpace(d.Title)
	d.Summary = strings.TrimSpace(d.Summary)
	d.Source = strings.TrimSpace(d.Source)
	d.TenantID = strings.TrimSpace(d.TenantID)
	d.ScopeType = strings.ToLower(strings.TrimSpace(d.ScopeType))
	d.ScopeID = strings.TrimSpace(d.ScopeID)

	if d.ID == "" {
		return errors.Join(ErrInvalidDocument, errors.New("id is required"))
	}
	if !validType(d.Type) {
		return errors.Join(ErrInvalidDocument, errors.New("type must be one of "+strings.Join(Types(), ", ")))
	}
	if d.Title == "" {
		return errors.Join(ErrInvalidDocument, errors.New("title is required"))
	}
	if d.Source == "" {
		return errors.Join(ErrInvalidDocument, errors.New("source is required"))
	}
	// A document with no permissions would be indexed and never returned.
	// Refusing it is better than silently accepting something invisible: the
	// publisher learns now rather than when someone reports that search is
	// missing records.
	permissions := make([]string, 0, len(d.Permissions))
	seen := map[string]bool{}
	for _, permission := range d.Permissions {
		permission = strings.TrimSpace(permission)
		if permission == "" || seen[permission] {
			continue
		}
		seen[permission] = true
		permissions = append(permissions, permission)
	}
	if len(permissions) == 0 {
		return errors.Join(ErrInvalidDocument, errors.New("at least one permission is required"))
	}
	d.Permissions = permissions

	if d.ScopeType == "" || d.ScopeID == "" {
		return errors.Join(ErrInvalidDocument, errors.New("scope_type and scope_id are required"))
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = now.UTC()
	} else {
		d.UpdatedAt = d.UpdatedAt.UTC()
	}
	return nil
}

func validType(value string) bool {
	for _, known := range Types() {
		if value == known {
			return true
		}
	}
	return false
}

// NormalizeTypes validates a type filter, reporting the first unknown type.
//
// An unknown type is refused rather than ignored. Dropping it would answer a
// question the caller did not ask — "everything" instead of "just this" — and
// they would read the result as though it were filtered.
func NormalizeTypes(values []string) ([]string, error) {
	types := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if !validType(value) {
			return nil, errors.New("unknown entity type: " + value)
		}
		types = append(types, value)
	}
	return types, nil
}

// Cursors.
//
// Search pages by offset rather than by a key. Results are ordered by
// relevance, which is a property of the query and the whole index rather than
// of any single document, so there is no stable key to resume from: the same
// document can legitimately move when another is indexed. An offset is the
// honest representation of "continue where the ranking left off", and the
// encoding stays opaque so it can be replaced when a provider offers
// something better.
const cursorPrefix = "s:"

// EncodeCursor returns the opaque cursor for a position.
func EncodeCursor(offset int) string {
	if offset <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.Itoa(offset)))
}

// DecodeCursor returns the offset a cursor names.
func DecodeCursor(cursor string) (int, error) {
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
	offset, err := strconv.Atoi(body)
	if err != nil || offset <= 0 {
		return 0, ErrInvalidCursor
	}
	return offset, nil
}

// Tokenize splits text into lowercase terms. It is exported because the
// in-memory provider and the tests that describe matching both depend on the
// same definition of a term.
func Tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r == '-' || r == '_' || isAlphanumeric(r))
	})
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "" {
			terms = append(terms, field)
		}
	}
	return terms
}

func isAlphanumeric(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	// Letters outside ASCII are terms too: the platform's data is not
	// English-only, and dropping them would make a Ukrainian client name
	// unsearchable.
	case r > 127:
		return true
	default:
		return false
	}
}
