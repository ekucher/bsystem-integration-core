package redmine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

func TestProjectsIssuesAndHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/current.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Redmine-API-Key") != "secret" {
			t.Fatalf("missing API key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":1,"login":"svc"}}`))
	})
	mux.HandleFunc("/projects.json", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "20" {
			t.Fatalf("unexpected limit: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":1,"projects":[{"id":7,"name":"Platform","identifier":"platform"}]}`))
	})
	mux.HandleFunc("/issues.json", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("project_id"); got != "platform" {
			t.Fatalf("unexpected project_id: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":1,"issues":[{"id":42,"subject":"Bug","project":{"id":7,"name":"Platform"},"status":{"id":1,"name":"New"}}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if health := client.Health(context.Background()); health.Status != "ready" {
		t.Fatalf("unexpected health: %#v", health)
	}
	projects, projectPage, err := client.ListProjects(context.Background(), adapters.Page{Limit: 20})
	if err != nil || len(projects) != 1 || projects[0].Identifier != "platform" {
		t.Fatalf("unexpected projects: %#v err=%v", projects, err)
	}
	if projectPage.Total != 1 || projectPage.NextCursor != "" {
		t.Fatalf("project page info = %+v, want the collection exhausted", projectPage)
	}
	issues, issuePage, err := client.ListIssues(context.Background(), "platform", adapters.Page{Limit: 20})
	if err != nil || len(issues) != 1 || issues[0].ID != 42 {
		t.Fatalf("unexpected issues: %#v err=%v", issues, err)
	}
	if issuePage.Total != 1 {
		t.Fatalf("issue page info = %+v", issuePage)
	}
}

// Filtering is applied by the upstream before paging, so the cursor walks the
// filtered collection rather than the whole one.
func TestIssuePaginationWalksTheFilteredCollection(t *testing.T) {
	subjects := []string{"one", "two", "three", "four", "five"}
	mux := http.NewServeMux()
	mux.HandleFunc("/issues.json", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("status_id"); got != "*" {
			t.Errorf("status_id = %q, want *: BSYSTEM shows the whole collection", got)
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		end := min(offset+limit, len(subjects))
		items := ""
		for i := offset; i < end; i++ {
			if items != "" {
				items += ","
			}
			items += fmt.Sprintf(`{"id":%d,"subject":%q,"project":{"id":7,"name":"P"},"status":{"id":1,"name":"New"}}`, 100+i, subjects[i])
		}
		_, _ = fmt.Fprintf(w, `{"total_count":%d,"issues":[%s]}`, len(subjects), items)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	var walked []string
	page := adapters.Page{Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		issues, info, err := client.ListIssues(context.Background(), "platform", page)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		if info.Total != len(subjects) {
			t.Fatalf("total = %d, want %d", info.Total, len(subjects))
		}
		for _, issue := range issues {
			walked = append(walked, issue.Subject)
		}
		if info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
	}
	if len(walked) != len(subjects) {
		t.Fatalf("walked %v, want every issue exactly once", walked)
	}
}

// Redmine's own maximum page size is 100; asking for more would be rejected
// upstream, so the platform clamps instead.
func TestPageSizeIsBoundedToRedminesMaximum(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{"total_count":0,"projects":[]}`))
	}))
	defer server.Close()
	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.ListProjects(context.Background(), adapters.Page{Limit: 5000}); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if got != "100" {
		t.Fatalf("limit = %q, want 100", got)
	}
}

func newDetailClient(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "101.json" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"project":{"id":101,"name":"Northwind Portal","identifier":"northwind-portal"}}`))
	})
	mux.HandleFunc("/issues/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "5001.json" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"issue":{"id":5001,"subject":"Design portal navigation","project":{"id":101,"name":"Northwind Portal"},"status":{"id":1,"name":"New"}}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return client
}

func TestGetProjectAndIssue(t *testing.T) {
	client := newDetailClient(t)

	project, err := client.GetProject(context.Background(), "101")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.Identifier != "northwind-portal" || project.Name != "Northwind Portal" {
		t.Fatalf("unexpected project: %#v", project)
	}

	issue, err := client.GetIssue(context.Background(), "5001")
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if issue.Subject == "" || issue.Project.ID != 101 || issue.Status.Name != "New" {
		t.Fatalf("unexpected issue: %#v", issue)
	}
}

func TestDetailReadsReportMissingRecordsAsNotFound(t *testing.T) {
	client := newDetailClient(t)
	tests := []struct {
		name string
		read func() error
	}{
		{name: "unknown project", read: func() error { _, err := client.GetProject(context.Background(), "999"); return err }},
		{name: "unknown issue", read: func() error { _, err := client.GetIssue(context.Background(), "9999"); return err }},
		{name: "empty project id", read: func() error { _, err := client.GetProject(context.Background(), " "); return err }},
		{name: "empty issue id", read: func() error { _, err := client.GetIssue(context.Background(), ""); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.read(); !errors.Is(err, adapters.ErrNotFound) {
				t.Fatalf("error = %v, want adapters.ErrNotFound", err)
			}
		})
	}
}
