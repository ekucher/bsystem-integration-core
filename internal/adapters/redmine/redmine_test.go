package redmine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

	client, err := New(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if health := client.Health(context.Background()); health.Status != "ready" {
		t.Fatalf("unexpected health: %#v", health)
	}
	projects, err := client.ListProjects(context.Background(), 20)
	if err != nil || len(projects) != 1 || projects[0].Identifier != "platform" {
		t.Fatalf("unexpected projects: %#v err=%v", projects, err)
	}
	issues, err := client.ListIssues(context.Background(), "platform", 20)
	if err != nil || len(issues) != 1 || issues[0].ID != 42 {
		t.Fatalf("unexpected issues: %#v err=%v", issues, err)
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
	client, err := New(server.URL, "secret", time.Second)
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
