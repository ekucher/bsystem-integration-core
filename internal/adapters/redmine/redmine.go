// Package redmine adapts the Redmine REST API to BSYSTEM.
//
// It isolates HUB and the normalized API from Redmine's conventions: nothing
// outside this package sees a Redmine field name, status code or envelope.
package redmine

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/httpx"
)

// Client reads projects and issues from Redmine.
type Client struct {
	http   *httpx.Client
	header http.Header
}

// Project is a Redmine project.
type Project struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Identifier  string `json:"identifier"`
	Description string `json:"description,omitempty"`
	Status      int    `json:"status,omitempty"`
}

// Issue is a Redmine issue, which BSYSTEM normalizes to a task.
type Issue struct {
	ID      int    `json:"id"`
	Subject string `json:"subject"`
	Project struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Status struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"status"`
}

type projectsResponse struct {
	Projects []Project `json:"projects"`
	Total    int       `json:"total_count"`
}

type issuesResponse struct {
	Issues []Issue `json:"issues"`
	Total  int     `json:"total_count"`
}

// Collection bounds. Redmine's own maximum page size is 100.
const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// New returns a client for the Redmine instance at rawURL.
func New(config adapters.Config) (*Client, error) {
	client, err := httpx.New(httpx.OptionsFor("redmine", config))
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	if key := strings.TrimSpace(config.APIKey); key != "" {
		header.Set("X-Redmine-API-Key", key)
	}
	return &Client{http: client, header: header}, nil
}

func (c *Client) Info() adapters.Info {
	return adapters.Info{ID: "redmine", Name: "Redmine", Version: "1", Status: adapters.StatusReady, Capabilities: []string{"projects.read", "issues.read"}}
}

// Health probes the endpoint Redmine uses to describe the calling user, which
// exercises both reachability and the credential.
func (c *Client) Health(ctx context.Context) adapters.Health {
	var out map[string]any
	if err := c.get(ctx, "/users/current.json", nil, &out); err != nil {
		return adapters.Health{Status: adapters.StatusDegraded, Message: err.Error()}
	}
	return adapters.Health{Status: adapters.StatusReady}
}

// BreakerState reports the adapter's circuit state.
func (c *Client) BreakerState() string { return string(c.http.BreakerState()) }

// ListProjects returns one page of projects.
func (c *Client) ListProjects(ctx context.Context, page adapters.Page) ([]Project, adapters.PageInfo, error) {
	offset, limit, err := page.Resolve(defaultPageSize, maxPageSize)
	if err != nil {
		return nil, adapters.PageInfo{}, err
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	var out projectsResponse
	if err := c.get(ctx, "/projects.json", query, &out); err != nil {
		return nil, adapters.PageInfo{}, err
	}
	return out.Projects, adapters.NewPageInfo(out.Total, offset, len(out.Projects)), nil
}

// ListIssues returns one page of issues, optionally restricted to a project
// by numeric id or identifier.
func (c *Client) ListIssues(ctx context.Context, projectID string, page adapters.Page) ([]Issue, adapters.PageInfo, error) {
	offset, limit, err := page.Resolve(defaultPageSize, maxPageSize)
	if err != nil {
		return nil, adapters.PageInfo{}, err
	}
	query := url.Values{
		"limit":  {strconv.Itoa(limit)},
		"offset": {strconv.Itoa(offset)},
		// Redmine hides closed issues unless asked; BSYSTEM shows the whole
		// collection and lets the caller filter.
		"status_id": {"*"},
	}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		query.Set("project_id", projectID)
	}
	var out issuesResponse
	if err := c.get(ctx, "/issues.json", query, &out); err != nil {
		return nil, adapters.PageInfo{}, err
	}
	return out.Issues, adapters.NewPageInfo(out.Total, offset, len(out.Issues)), nil
}

// GetProject reads one project by numeric id or identifier. It returns
// adapters.ErrNotFound when the upstream has no such record.
func (c *Client) GetProject(ctx context.Context, id string) (Project, error) {
	var out struct {
		Project Project `json:"project"`
	}
	if err := c.getByID(ctx, "/projects/", id, &out); err != nil {
		return Project{}, err
	}
	return out.Project, nil
}

// GetIssue reads one issue. It returns adapters.ErrNotFound when the upstream
// has no such record.
func (c *Client) GetIssue(ctx context.Context, id string) (Issue, error) {
	var out struct {
		Issue Issue `json:"issue"`
	}
	if err := c.getByID(ctx, "/issues/", id, &out); err != nil {
		return Issue{}, err
	}
	return out.Issue, nil
}

func (c *Client) getByID(ctx context.Context, prefix, id string, out any) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return adapters.ErrNotFound
	}
	return c.get(ctx, prefix+url.PathEscape(id)+".json", nil, out)
}

// get issues an idempotent read. Every Redmine call BSYSTEM makes is a read,
// so all of them may be retried.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.http.Do(ctx, httpx.Request{
		Method:     http.MethodGet,
		Path:       path,
		Query:      query,
		Header:     c.header,
		Idempotent: true,
	}, out)
}
