package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/espocrm"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/outline"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/redmine"
	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

type crmReader interface {
	ListAccounts(context.Context, adapters.Page) ([]espocrm.Account, adapters.PageInfo, error)
	ListContacts(context.Context, adapters.Page) ([]espocrm.Contact, adapters.PageInfo, error)
}
type projectReader interface {
	ListProjects(context.Context, adapters.Page) ([]redmine.Project, adapters.PageInfo, error)
	ListIssues(context.Context, string, adapters.Page) ([]redmine.Issue, adapters.PageInfo, error)
}
type documentReader interface {
	ListDocuments(context.Context, adapters.Page) ([]outline.Document, adapters.PageInfo, error)
}

// collection is the envelope every normalized list returns. The pagination
// block is what lets a caller walk a collection without guessing whether more
// remains.
type collection[T any] struct {
	Data       []T            `json:"data"`
	Pagination paginationView `json:"pagination"`
}

type paginationView struct {
	// Total is the size of the whole collection as the source system reports
	// it, not the size of this page.
	Total int `json:"total"`
	// Limit is the page size actually applied, which may be smaller than the
	// one requested.
	Limit int `json:"limit"`
	// NextCursor is absent once the collection is exhausted.
	NextCursor string `json:"next_cursor,omitempty"`
}

type clientView struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Website  string `json:"website,omitempty"`
	Email    string `json:"email,omitempty"`
	Phone    string `json:"phone,omitempty"`
}
type contactView struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	ClientID string `json:"client_id,omitempty"`
	Email    string `json:"email,omitempty"`
	Phone    string `json:"phone,omitempty"`
}
type projectView struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	SourceID    string `json:"source_id"`
	Name        string `json:"name"`
	Identifier  string `json:"identifier"`
	Description string `json:"description,omitempty"`
}
type issueView struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	SourceID  string `json:"source_id"`
	Subject   string `json:"subject"`
	ProjectID string `json:"project_id,omitempty"`
	Status    string `json:"status,omitempty"`
}
type documentView struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	SourceID     string `json:"source_id"`
	Title        string `json:"title"`
	URL          string `json:"url,omitempty"`
	CollectionID string `json:"collection_id,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// requestPage reads the pagination parameters. A malformed limit falls back
// to the default and an out-of-range one is clamped by the adapter, because a
// caller asking for more than the platform serves should get the maximum
// rather than an error. A cursor the platform did not issue is rejected: it
// cannot be interpreted, and guessing at it would silently return the wrong
// window.
func requestPage(w http.ResponseWriter, r *http.Request) (adapters.Page, bool) {
	limit, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if err != nil {
		limit = 0
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if _, err := adapters.DecodeCursor(cursor); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pagination cursor", "code": "invalid_cursor"})
		return adapters.Page{}, false
	}
	return adapters.Page{Limit: limit, Cursor: cursor}, true
}

// writeCollection renders a normalized collection.
func writeCollection[T any](w http.ResponseWriter, items []T, info adapters.PageInfo) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, collection[T]{
		Data:       items,
		Pagination: paginationView{Total: info.Total, Limit: len(items), NextCursor: info.NextCursor},
	})
}
func upstreamFailure(w http.ResponseWriter, adapter string, err error) {
	log.Printf("%s upstream request failed: %v", adapter, err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream service unavailable", "code": "upstream_unavailable", "source": adapter})
}

// adapterFor resolves a registered adapter and asserts the capability the
// handler needs. An adapter that is absent or lacks the capability is a
// configuration problem, not an upstream failure, so it is reported as such.
func adapterFor[T any](w http.ResponseWriter, id, name string) (T, bool) {
	var zero T
	adapter, registered := adapterRegistry.Get(id)
	if !registered {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": name + " adapter not configured"})
		return zero, false
	}
	reader, capable := adapter.(T)
	if !capable {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": name + " adapter does not support this capability"})
		return zero, false
	}
	return reader, true
}

// mapGlobalID allocates or returns the Global ID for one upstream record.
// A mapping failure is fatal to the request: returning the record without its
// platform identity would hand the caller an unaddressable entity.
func (a *app) mapGlobalID(w http.ResponseWriter, r *http.Request, entityType, source, sourceID string, metadata map[string]any) (string, bool) {
	entity, err := a.db.CreateGlobalEntity(r.Context(), entityType, source, sourceID, "", metadata)
	if err != nil {
		log.Printf("Global ID mapping failed for %s/%s: %v", source, entityType, err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Global ID mapping failed"})
		return "", false
	}
	return entity.GlobalID, true
}

func (a *app) listClients(w http.ResponseWriter, r *http.Request) {
	reader, ok := adapterFor[crmReader](w, "espocrm", "EspoCRM")
	if !ok {
		return
	}
	page, ok := requestPage(w, r)
	if !ok {
		return
	}
	accounts, info, err := reader.ListAccounts(r.Context(), page)
	if err != nil {
		upstreamFailure(w, "espocrm", err)
		return
	}
	result := make([]clientView, 0, len(accounts))
	for _, item := range accounts {
		globalID, ok := a.mapGlobalID(w, r, "client", "espocrm", item.ID, map[string]any{"name": item.Name})
		if !ok {
			return
		}
		result = append(result, clientView{ID: globalID, Source: "espocrm", SourceID: item.ID, Name: item.Name, Website: item.Website, Email: item.Email, Phone: item.Phone})
	}
	writeCollection(w, result, info)
}

func (a *app) listContacts(w http.ResponseWriter, r *http.Request) {
	reader, ok := adapterFor[crmReader](w, "espocrm", "EspoCRM")
	if !ok {
		return
	}
	page, ok := requestPage(w, r)
	if !ok {
		return
	}
	contacts, info, err := reader.ListContacts(r.Context(), page)
	if err != nil {
		upstreamFailure(w, "espocrm", err)
		return
	}
	result := make([]contactView, 0, len(contacts))
	for _, item := range contacts {
		globalID, ok := a.mapGlobalID(w, r, "contact", "espocrm", item.ID, map[string]any{"name": item.Name})
		if !ok {
			return
		}
		// The owning client is mapped here because the contact listing is
		// where that relationship first becomes known to the platform.
		clientID := ""
		if item.AccountID != "" {
			if mapped, mapErr := a.db.CreateGlobalEntity(r.Context(), "client", "espocrm", item.AccountID, "", nil); mapErr == nil {
				clientID = mapped.GlobalID
			}
		}
		result = append(result, contactView{ID: globalID, Source: "espocrm", SourceID: item.ID, Name: item.Name, ClientID: clientID, Email: item.Email, Phone: item.Phone})
	}
	writeCollection(w, result, info)
}

func (a *app) listProjects(w http.ResponseWriter, r *http.Request) {
	reader, ok := adapterFor[projectReader](w, "redmine", "Redmine")
	if !ok {
		return
	}
	page, ok := requestPage(w, r)
	if !ok {
		return
	}
	projects, info, err := reader.ListProjects(r.Context(), page)
	if err != nil {
		upstreamFailure(w, "redmine", err)
		return
	}
	result := make([]projectView, 0, len(projects))
	for _, item := range projects {
		sourceID := strconv.Itoa(item.ID)
		globalID, ok := a.mapGlobalID(w, r, "project", "redmine", sourceID, map[string]any{"identifier": item.Identifier, "name": item.Name})
		if !ok {
			return
		}
		result = append(result, projectView{ID: globalID, Source: "redmine", SourceID: sourceID, Name: item.Name, Identifier: item.Identifier, Description: item.Description})
	}
	writeCollection(w, result, info)
}

func (a *app) listIssues(w http.ResponseWriter, r *http.Request) {
	reader, ok := adapterFor[projectReader](w, "redmine", "Redmine")
	if !ok {
		return
	}
	page, ok := requestPage(w, r)
	if !ok {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	issues, info, err := reader.ListIssues(r.Context(), project, page)
	if err != nil {
		upstreamFailure(w, "redmine", err)
		return
	}
	result := make([]issueView, 0, len(issues))
	for _, item := range issues {
		sourceID := strconv.Itoa(item.ID)
		globalID, ok := a.mapGlobalID(w, r, "task", "redmine", sourceID, map[string]any{"subject": item.Subject})
		if !ok {
			return
		}
		projectID := ""
		if item.Project.ID > 0 {
			if mapped, mapErr := a.db.CreateGlobalEntity(r.Context(), "project", "redmine", strconv.Itoa(item.Project.ID), "", map[string]any{"name": item.Project.Name}); mapErr == nil {
				projectID = mapped.GlobalID
			}
		}
		result = append(result, issueView{ID: globalID, Source: "redmine", SourceID: sourceID, Subject: item.Subject, ProjectID: projectID, Status: item.Status.Name})
	}
	writeCollection(w, result, info)
}

func (a *app) listDocuments(w http.ResponseWriter, r *http.Request) {
	reader, ok := adapterFor[documentReader](w, "outline", "Outline")
	if !ok {
		return
	}
	page, ok := requestPage(w, r)
	if !ok {
		return
	}
	documents, info, err := reader.ListDocuments(r.Context(), page)
	if err != nil {
		upstreamFailure(w, "outline", err)
		return
	}
	result := make([]documentView, 0, len(documents))
	for _, item := range documents {
		globalID, ok := a.mapGlobalID(w, r, "document", "outline", item.ID, map[string]any{"title": item.Title, "collection_id": item.CollectionID})
		if !ok {
			return
		}
		result = append(result, documentView{ID: globalID, Source: "outline", SourceID: item.ID, Title: item.Title, URL: item.URL, CollectionID: item.CollectionID, UpdatedAt: item.UpdatedAt})
	}
	writeCollection(w, result, info)
}

// --- Detail endpoints -------------------------------------------------------

type crmDetailReader interface {
	GetAccount(context.Context, string) (espocrm.Account, error)
	GetContact(context.Context, string) (espocrm.Contact, error)
}
type projectDetailReader interface {
	GetProject(context.Context, string) (redmine.Project, error)
	GetIssue(context.Context, string) (redmine.Issue, error)
}
type documentDetailReader interface {
	GetDocument(context.Context, string) (outline.Document, error)
}

// notFound is the single normalized answer for "no such resource, or you may
// not learn that there is one". Both cases must look identical from outside.
func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "resource not found", "code": "not_found"})
}

// upstreamRead maps an adapter error onto the platform's response. A missing
// upstream record is a 404, not an upstream failure.
func upstreamRead(w http.ResponseWriter, adapter string, err error) {
	if errors.Is(err, adapters.ErrNotFound) {
		notFound(w)
		return
	}
	upstreamFailure(w, adapter, err)
}

// resolveScopedEntity turns the Global ID in the request path into its
// upstream mapping, having established that the caller may reach that specific
// resource.
//
// It reports whether the handler may proceed, having already written the
// response when it may not. Three refusals are deliberately indistinguishable
// from outside — an unknown Global ID, a Global ID of a different entity type,
// and a resource a scope-confined caller has not been granted — so that the
// endpoint cannot be used to discover which identifiers exist or who owns
// them.
func (a *app) resolveScopedEntity(w http.ResponseWriter, r *http.Request, entityType, permission, scopeType string) (platformdb.GlobalEntity, bool) {
	principal := principalFrom(r.Context())
	confined := a.authz.IsConfined(principal)

	// A caller who cannot hold the permission anywhere is refused before any
	// lookup happens, so the endpoint reveals nothing to them at all. A
	// scope-confined caller is exempt: its authority is per-resource, so it
	// has to be evaluated against the resolved resource instead.
	if !confined && !a.authorizeResource(w, r, permission, authz.ScopeGlobal, "*") {
		return platformdb.GlobalEntity{}, false
	}

	entity, err := a.db.ResolveGlobalEntity(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil || entity.EntityType != entityType {
		notFound(w)
		return platformdb.GlobalEntity{}, false
	}

	decision, err := a.authz.Evaluate(r.Context(), principal, permission, authz.Resource(scopeType, entity.GlobalID))
	if err != nil {
		log.Printf("authorization evaluation failed: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization store unavailable"})
		return platformdb.GlobalEntity{}, false
	}
	if !decision.Allowed {
		if confined {
			notFound(w)
		} else {
			writeDenied(w, decision)
		}
		return platformdb.GlobalEntity{}, false
	}
	return entity, true
}

func (a *app) getClient(w http.ResponseWriter, r *http.Request) {
	entity, ok := a.resolveScopedEntity(w, r, "client", "crm.client.read", authz.ScopeClient)
	if !ok {
		return
	}
	reader, ok := adapterFor[crmDetailReader](w, "espocrm", "EspoCRM")
	if !ok {
		return
	}
	account, err := reader.GetAccount(r.Context(), entity.SourceID)
	if err != nil {
		upstreamRead(w, "espocrm", err)
		return
	}
	writeJSON(w, http.StatusOK, clientView{ID: entity.GlobalID, Source: entity.Source, SourceID: entity.SourceID, Name: account.Name, Website: account.Website, Email: account.Email, Phone: account.Phone})
}

func (a *app) getContact(w http.ResponseWriter, r *http.Request) {
	entity, ok := a.resolveScopedEntity(w, r, "contact", "crm.client.read", authz.ScopeResource)
	if !ok {
		return
	}
	reader, ok := adapterFor[crmDetailReader](w, "espocrm", "EspoCRM")
	if !ok {
		return
	}
	contact, err := reader.GetContact(r.Context(), entity.SourceID)
	if err != nil {
		upstreamRead(w, "espocrm", err)
		return
	}
	// The owning client is reported only when it is already mapped. An
	// unmapped owner is omitted rather than allocated here, so reading a
	// contact cannot mint identifiers as a side effect.
	clientID := ""
	if contact.AccountID != "" {
		if mapped, mapErr := a.db.LookupGlobalEntity(r.Context(), "client", "espocrm", contact.AccountID); mapErr == nil {
			clientID = mapped.GlobalID
		}
	}
	writeJSON(w, http.StatusOK, contactView{ID: entity.GlobalID, Source: entity.Source, SourceID: entity.SourceID, Name: contact.Name, ClientID: clientID, Email: contact.Email, Phone: contact.Phone})
}

func (a *app) getProject(w http.ResponseWriter, r *http.Request) {
	entity, ok := a.resolveScopedEntity(w, r, "project", "projects.task.read", authz.ScopeProject)
	if !ok {
		return
	}
	reader, ok := adapterFor[projectDetailReader](w, "redmine", "Redmine")
	if !ok {
		return
	}
	project, err := reader.GetProject(r.Context(), entity.SourceID)
	if err != nil {
		upstreamRead(w, "redmine", err)
		return
	}
	writeJSON(w, http.StatusOK, projectView{ID: entity.GlobalID, Source: entity.Source, SourceID: entity.SourceID, Name: project.Name, Identifier: project.Identifier, Description: project.Description})
}

func (a *app) getIssue(w http.ResponseWriter, r *http.Request) {
	entity, ok := a.resolveScopedEntity(w, r, "task", "projects.task.read", authz.ScopeResource)
	if !ok {
		return
	}
	reader, ok := adapterFor[projectDetailReader](w, "redmine", "Redmine")
	if !ok {
		return
	}
	issue, err := reader.GetIssue(r.Context(), entity.SourceID)
	if err != nil {
		upstreamRead(w, "redmine", err)
		return
	}
	projectID := ""
	if issue.Project.ID > 0 {
		if mapped, mapErr := a.db.LookupGlobalEntity(r.Context(), "project", "redmine", strconv.Itoa(issue.Project.ID)); mapErr == nil {
			projectID = mapped.GlobalID
		}
	}
	writeJSON(w, http.StatusOK, issueView{ID: entity.GlobalID, Source: entity.Source, SourceID: entity.SourceID, Subject: issue.Subject, ProjectID: projectID, Status: issue.Status.Name})
}

func (a *app) getDocument(w http.ResponseWriter, r *http.Request) {
	entity, ok := a.resolveScopedEntity(w, r, "document", "wiki.document.read", authz.ScopeResource)
	if !ok {
		return
	}
	reader, ok := adapterFor[documentDetailReader](w, "outline", "Outline")
	if !ok {
		return
	}
	document, err := reader.GetDocument(r.Context(), entity.SourceID)
	if err != nil {
		upstreamRead(w, "outline", err)
		return
	}
	writeJSON(w, http.StatusOK, documentView{ID: entity.GlobalID, Source: entity.Source, SourceID: entity.SourceID, Title: document.Title, URL: document.URL, CollectionID: document.CollectionID, UpdatedAt: document.UpdatedAt})
}
