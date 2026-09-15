package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/adapters/espocrm"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/outline"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/redmine"
)

type crmReader interface {
	ListAccounts(context.Context, int) ([]espocrm.Account, error)
	ListContacts(context.Context, int) ([]espocrm.Contact, error)
}
type projectReader interface {
	ListProjects(context.Context, int) ([]redmine.Project, error)
	ListIssues(context.Context, string, int) ([]redmine.Issue, error)
}
type documentReader interface {
	ListDocuments(context.Context, int) ([]outline.Document, error)
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

func requestLimit(r *http.Request, defaultValue int) int {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		return defaultValue
	}
	if limit > 200 {
		return 200
	}
	return limit
}
func upstreamFailure(w http.ResponseWriter, adapter string, err error) {
	log.Printf("%s upstream request failed: %v", adapter, err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream service unavailable", "code": "upstream_unavailable", "source": adapter})
}

func (a *app) listClients(w http.ResponseWriter, r *http.Request) {
	adapter, ok := adapterRegistry.Get("espocrm")
	reader, okReader := adapter.(crmReader)
	if !ok || !okReader {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "EspoCRM adapter not configured"})
		return
	}
	accounts, err := reader.ListAccounts(r.Context(), requestLimit(r, 50))
	if err != nil {
		upstreamFailure(w, "espocrm", err)
		return
	}
	result := make([]clientView, 0, len(accounts))
	for _, item := range accounts {
		entity, err := a.db.CreateGlobalEntity(r.Context(), "client", "espocrm", item.ID, "", map[string]any{"name": item.Name})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Global ID mapping failed"})
			return
		}
		result = append(result, clientView{ID: entity.GlobalID, Source: "espocrm", SourceID: item.ID, Name: item.Name, Website: item.Website, Email: item.Email, Phone: item.Phone})
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) listContacts(w http.ResponseWriter, r *http.Request) {
	adapter, ok := adapterRegistry.Get("espocrm")
	reader, okReader := adapter.(crmReader)
	if !ok || !okReader {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "EspoCRM adapter not configured"})
		return
	}
	contacts, err := reader.ListContacts(r.Context(), requestLimit(r, 50))
	if err != nil {
		upstreamFailure(w, "espocrm", err)
		return
	}
	result := make([]contactView, 0, len(contacts))
	for _, item := range contacts {
		entity, err := a.db.CreateGlobalEntity(r.Context(), "contact", "espocrm", item.ID, "", map[string]any{"name": item.Name})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Global ID mapping failed"})
			return
		}
		clientID := ""
		if item.AccountID != "" {
			if mapped, mapErr := a.db.CreateGlobalEntity(r.Context(), "client", "espocrm", item.AccountID, "", nil); mapErr == nil {
				clientID = mapped.GlobalID
			}
		}
		result = append(result, contactView{ID: entity.GlobalID, Source: "espocrm", SourceID: item.ID, Name: item.Name, ClientID: clientID, Email: item.Email, Phone: item.Phone})
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) listProjects(w http.ResponseWriter, r *http.Request) {
	adapter, ok := adapterRegistry.Get("redmine")
	reader, okReader := adapter.(projectReader)
	if !ok || !okReader {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Redmine adapter not configured"})
		return
	}
	projects, err := reader.ListProjects(r.Context(), requestLimit(r, 50))
	if err != nil {
		upstreamFailure(w, "redmine", err)
		return
	}
	result := make([]projectView, 0, len(projects))
	for _, item := range projects {
		sourceID := strconv.Itoa(item.ID)
		entity, err := a.db.CreateGlobalEntity(r.Context(), "project", "redmine", sourceID, "", map[string]any{"identifier": item.Identifier, "name": item.Name})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Global ID mapping failed"})
			return
		}
		result = append(result, projectView{ID: entity.GlobalID, Source: "redmine", SourceID: sourceID, Name: item.Name, Identifier: item.Identifier, Description: item.Description})
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) listIssues(w http.ResponseWriter, r *http.Request) {
	adapter, ok := adapterRegistry.Get("redmine")
	reader, okReader := adapter.(projectReader)
	if !ok || !okReader {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Redmine adapter not configured"})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	issues, err := reader.ListIssues(r.Context(), project, requestLimit(r, 50))
	if err != nil {
		upstreamFailure(w, "redmine", err)
		return
	}
	result := make([]issueView, 0, len(issues))
	for _, item := range issues {
		sourceID := strconv.Itoa(item.ID)
		entity, err := a.db.CreateGlobalEntity(r.Context(), "task", "redmine", sourceID, "", map[string]any{"subject": item.Subject})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Global ID mapping failed"})
			return
		}
		projectID := ""
		if item.Project.ID > 0 {
			if mapped, mapErr := a.db.CreateGlobalEntity(r.Context(), "project", "redmine", strconv.Itoa(item.Project.ID), "", map[string]any{"name": item.Project.Name}); mapErr == nil {
				projectID = mapped.GlobalID
			}
		}
		result = append(result, issueView{ID: entity.GlobalID, Source: "redmine", SourceID: sourceID, Subject: item.Subject, ProjectID: projectID, Status: item.Status.Name})
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) listDocuments(w http.ResponseWriter, r *http.Request) {
	adapter, ok := adapterRegistry.Get("outline")
	reader, okReader := adapter.(documentReader)
	if !ok || !okReader {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Outline adapter not configured"})
		return
	}
	documents, err := reader.ListDocuments(r.Context(), requestLimit(r, 50))
	if err != nil {
		upstreamFailure(w, "outline", err)
		return
	}
	result := make([]documentView, 0, len(documents))
	for _, item := range documents {
		entity, err := a.db.CreateGlobalEntity(r.Context(), "document", "outline", item.ID, "", map[string]any{"title": item.Title, "collection_id": item.CollectionID})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Global ID mapping failed"})
			return
		}
		result = append(result, documentView{ID: entity.GlobalID, Source: "outline", SourceID: item.ID, Title: item.Title, URL: item.URL, CollectionID: item.CollectionID, UpdatedAt: item.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, result)
}
