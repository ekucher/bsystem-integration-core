package redmine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

type Client struct {
	baseURL *url.URL
	apiKey  string
	http    *http.Client
}

type Project struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Identifier  string `json:"identifier"`
	Description string `json:"description,omitempty"`
	Status      int    `json:"status,omitempty"`
}

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

func New(rawURL, apiKey string, timeout time.Duration) (*Client, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("Redmine base URL is required")
	}
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("invalid Redmine base URL")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{baseURL: u, apiKey: strings.TrimSpace(apiKey), http: &http.Client{Timeout: timeout}}, nil
}

func (c *Client) Info() adapters.Info {
	return adapters.Info{ID: "redmine", Name: "Redmine", Version: "1", Status: adapters.StatusReady, Capabilities: []string{"projects.read", "issues.read"}}
}

func (c *Client) Health(ctx context.Context) adapters.Health {
	var out map[string]any
	if err := c.getJSON(ctx, "/users/current.json", nil, &out); err != nil {
		return adapters.Health{Status: adapters.StatusDegraded, Message: err.Error()}
	}
	return adapters.Health{Status: adapters.StatusReady}
}

func (c *Client) ListProjects(ctx context.Context, limit int) ([]Project, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	var out projectsResponse
	if err := c.getJSON(ctx, "/projects.json", q, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

func (c *Client) ListIssues(ctx context.Context, projectID string, limit int) ([]Issue, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}, "status_id": {"*"}}
	if strings.TrimSpace(projectID) != "" {
		q.Set("project_id", strings.TrimSpace(projectID))
	}
	var out issuesResponse
	if err := c.getJSON(ctx, "/issues.json", q, &out); err != nil {
		return nil, err
	}
	return out.Issues, nil
}

// GetProject reads one project by numeric id or identifier. It returns
// adapters.ErrNotFound when the upstream has no such record.
func (c *Client) GetProject(ctx context.Context, id string) (Project, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Project{}, adapters.ErrNotFound
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := c.getJSON(ctx, "/projects/"+url.PathEscape(id)+".json", nil, &out); err != nil {
		return Project{}, err
	}
	return out.Project, nil
}

// GetIssue reads one issue. It returns adapters.ErrNotFound when the upstream
// has no such record.
func (c *Client) GetIssue(ctx context.Context, id string) (Issue, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Issue{}, adapters.ErrNotFound
	}
	var out struct {
		Issue Issue `json:"issue"`
	}
	if err := c.getJSON(ctx, "/issues/"+url.PathEscape(id)+".json", nil, &out); err != nil {
		return Issue{}, err
	}
	return out.Issue, nil
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Redmine-API-Key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Redmine request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return adapters.ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Redmine returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode Redmine response: %w", err)
	}
	return nil
}
