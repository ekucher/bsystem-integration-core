package outline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

// documentsListBody is the envelope Outline actually returns from
// documents.list: "data" is an array of documents, with paging metadata
// alongside it rather than nested inside it.
const documentsListBody = `{"data":[{"id":"doc-1","title":"Runbook","collectionId":"col-1","updatedAt":"2026-01-06T11:30:00.000Z"},{"id":"doc-2","title":"Onboarding","collectionId":"col-1"}],"pagination":{"offset":0,"limit":2,"total":2}}`

func newTestClient(t *testing.T, mux *http.ServeMux) *Client {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "test-outline-key", time.Second)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return client
}

func TestListDocuments(t *testing.T) {
	var receivedLimit float64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents.list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-outline-key" {
			t.Errorf("missing or wrong bearer credential")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		receivedLimit, _ = body["limit"].(float64)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(documentsListBody))
	})

	client := newTestClient(t, mux)
	documents, info, err := client.ListDocuments(context.Background(), adapters.Page{Limit: 2})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("returned %d documents, want 2", len(documents))
	}
	if documents[0].ID != "doc-1" || documents[0].Title != "Runbook" || documents[0].CollectionID != "col-1" {
		t.Fatalf("unexpected first document: %#v", documents[0])
	}
	if documents[0].UpdatedAt == "" {
		t.Error("updatedAt must be preserved; the normalized API exposes it")
	}
	if receivedLimit != 2 {
		t.Fatalf("limit sent upstream = %v, want 2", receivedLimit)
	}
	if info.Total != 2 || info.NextCursor != "" {
		t.Fatalf("page info = %+v, want the collection exhausted", info)
	}
}

// Outline reads over POST, so the offset travels in the request body rather
// than the query string.
func TestListDocumentsPaginates(t *testing.T) {
	ids := []string{"d1", "d2", "d3", "d4", "d5"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents.list", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		end := min(body.Offset+body.Limit, len(ids))
		items := ""
		for i := body.Offset; i < end; i++ {
			if items != "" {
				items += ","
			}
			items += `{"id":"` + ids[i] + `","title":"t"}`
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s],"pagination":{"offset":%d,"limit":%d,"total":%d}}`, items, body.Offset, body.Limit, len(ids))
	})
	client := newTestClient(t, mux)

	var walked []string
	page := adapters.Page{Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		documents, info, err := client.ListDocuments(context.Background(), page)
		if err != nil {
			t.Fatalf("list documents: %v", err)
		}
		for _, document := range documents {
			walked = append(walked, document.ID)
		}
		if info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
	}
	if len(walked) != len(ids) {
		t.Fatalf("walked %v, want every document exactly once", walked)
	}
}

func TestSearchDocuments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents.search", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Query != "runbook" {
			t.Errorf("query = %q, want runbook", body.Query)
		}
		_, _ = w.Write([]byte(`{"data":[{"context":"…runbook…","ranking":0.9,"document":{"id":"doc-1","title":"Runbook"}}],"pagination":{"offset":0,"limit":1,"total":1}}`))
	})
	client := newTestClient(t, mux)

	hits, info, err := client.SearchDocuments(context.Background(), "runbook", adapters.Page{Limit: 10})
	if err != nil {
		t.Fatalf("search documents: %v", err)
	}
	if len(hits) != 1 || hits[0].Document.Title != "Runbook" || hits[0].Context == "" {
		t.Fatalf("unexpected hits: %#v", hits)
	}
	if info.Total != 1 {
		t.Fatalf("page info = %+v", info)
	}
	if _, _, err := client.SearchDocuments(context.Background(), "  ", adapters.Page{}); err == nil {
		t.Fatal("an empty query must be rejected before the upstream call")
	}
}

func TestListDocumentsBoundsTheLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLimit float64
	}{
		{name: "zero falls back", limit: 0, wantLimit: 50},
		{name: "negative falls back", limit: -5, wantLimit: 50},
		{name: "in range is kept", limit: 10, wantLimit: 10},
		// An over-large request is clamped to what the platform will serve,
		// not rejected and not silently reduced to the default.
		{name: "above the cap is clamped", limit: 5000, wantLimit: 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received float64
			mux := http.NewServeMux()
			mux.HandleFunc("/api/documents.list", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				received, _ = body["limit"].(float64)
				_, _ = w.Write([]byte(`{"data":[],"pagination":{"offset":0,"limit":0,"total":0}}`))
			})
			client := newTestClient(t, mux)
			if _, _, err := client.ListDocuments(context.Background(), adapters.Page{Limit: test.limit}); err != nil {
				t.Fatalf("list documents: %v", err)
			}
			if received != test.wantLimit {
				t.Fatalf("limit sent upstream = %v, want %v", received, test.wantLimit)
			}
		})
	}
}

func TestGetDocument(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents.info", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["id"] != "doc-1" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"doc-1","title":"Runbook","text":"hello","collectionId":"col-1"}}`))
	})
	client := newTestClient(t, mux)

	document, err := client.GetDocument(context.Background(), "doc-1")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if document.Text != "hello" || document.Title != "Runbook" {
		t.Fatalf("unexpected document: %#v", document)
	}

	if _, err := client.GetDocument(context.Background(), ""); err == nil {
		t.Fatal("an empty document id must be rejected before the upstream call")
	}
	if _, err := client.GetDocument(context.Background(), "doc-missing"); err == nil {
		t.Fatal("an upstream 404 must surface as an error")
	}
}

func TestHealth(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus string
	}{
		{
			name:       "ready",
			handler:    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(documentsListBody)) },
			wantStatus: "ready",
		},
		{
			name:       "upstream rejects the credential",
			handler:    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			wantStatus: "degraded",
		},
		{
			name:       "upstream is rate limiting",
			handler:    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
			wantStatus: "degraded",
		},
		{
			name:       "upstream returns an unreadable body",
			handler:    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":`)) },
			wantStatus: "degraded",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/documents.list", test.handler)
			health := newTestClient(t, mux).Health(context.Background())
			if string(health.Status) != test.wantStatus {
				t.Fatalf("status = %q, want %q (%s)", health.Status, test.wantStatus, health.Message)
			}
		})
	}
}

// An upstream failure must never surface the API key, because the adapter
// error text reaches logs and, in normalized form, the platform boundary.
func TestErrorsDoNotLeakTheCredential(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents.list", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := newTestClient(t, mux)
	_, _, err := client.ListDocuments(context.Background(), adapters.Page{Limit: 10})
	if err == nil {
		t.Fatal("an upstream 500 must surface as an error")
	}
	if strings.Contains(err.Error(), "test-outline-key") {
		t.Fatalf("adapter error leaked the credential: %v", err)
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		apiKey  string
		wantErr bool
	}{
		{name: "valid", url: "https://outline.example.invalid", apiKey: "k"},
		{name: "trailing slash is trimmed", url: "https://outline.example.invalid/", apiKey: "k"},
		{name: "empty url", url: "", apiKey: "k", wantErr: true},
		{name: "url without a scheme", url: "outline.example.invalid", apiKey: "k", wantErr: true},
		{name: "missing api key", url: "https://outline.example.invalid", apiKey: "", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.url, test.apiKey, time.Second)
			if test.wantErr != (err != nil) {
				t.Fatalf("New() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}
