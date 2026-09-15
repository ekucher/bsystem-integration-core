package outline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListAndGetDocument(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents.list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("missing bearer auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"documents":[{"id":"doc-1","title":"Runbook","collectionId":"col-1"}]}}`))
	})
	mux.HandleFunc("/api/documents.info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"doc-1","title":"Runbook","text":"hello"}}`))
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
	docs, err := client.ListDocuments(context.Background(), 10)
	if err != nil || len(docs) != 1 || docs[0].Title != "Runbook" {
		t.Fatalf("unexpected docs: %#v err=%v", docs, err)
	}
	doc, err := client.GetDocument(context.Background(), "doc-1")
	if err != nil || doc.Text != "hello" {
		t.Fatalf("unexpected doc: %#v err=%v", doc, err)
	}
}
