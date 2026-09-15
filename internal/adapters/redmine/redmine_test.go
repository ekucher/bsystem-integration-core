package redmine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
