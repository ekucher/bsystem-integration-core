package search

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

// fakeCluster records what the provider sent and replies with what a real
// cluster would. It is a contract test: no BSYSTEM deployment runs
// OpenSearch yet, so the requests are all that can be verified, and they are
// what the first person pointing this at a cluster will depend on.
type fakeCluster struct {
	*httptest.Server
	requests []recorded
}

type recorded struct {
	Method      string
	Path        string
	ContentType string
	Body        string
}

func newCluster(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeCluster {
	t.Helper()
	cluster := &fakeCluster{}
	cluster.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cluster.requests = append(cluster.requests, recorded{
			Method: r.Method, Path: r.URL.Path,
			ContentType: r.Header.Get("Content-Type"), Body: string(body),
		})
		handler(w, r)
	}))
	t.Cleanup(cluster.Close)
	return cluster
}

func newProvider(t *testing.T, cluster *fakeCluster) *OpenSearch {
	t.Helper()
	provider, err := NewOpenSearch(adapters.Config{BaseURL: cluster.URL, Timeout: 2 * time.Second}, "test-index")
	if err != nil {
		t.Fatalf("NewOpenSearch: %v", err)
	}
	return provider
}

func TestOpenSearchRequiresABaseURL(t *testing.T) {
	if _, err := NewOpenSearch(adapters.Config{}, ""); err == nil {
		t.Error("a provider was built without a cluster to talk to")
	}
}

// Bulk indexing must be newline-delimited JSON keyed by the Global ID. A JSON
// array is rejected by the cluster, and an index without an explicit id
// accumulates a new copy on every re-index.
func TestBulkIndexingIsNDJSONKeyedByGlobalID(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(bulkResponse{Errors: false})
	})
	provider := newProvider(t, cluster)

	err := provider.Index(context.Background(),
		document("CL-000001", TypeClient, "Northwind", "Key account", "crm.client.read"),
		document("PR-000001", TypeProject, "Migration", "", "projects.task.read"),
	)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(cluster.requests) != 1 {
		t.Fatalf("%d requests, want one bulk call", len(cluster.requests))
	}
	sent := cluster.requests[0]
	if sent.Method != http.MethodPost || sent.Path != "/_bulk" {
		t.Errorf("%s %s, want POST /_bulk", sent.Method, sent.Path)
	}
	if sent.ContentType != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson: a cluster rejects an array here", sent.ContentType)
	}
	lines := strings.Split(strings.TrimRight(sent.Body, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("%d lines, want an action and a document for each of two records:\n%s", len(lines), sent.Body)
	}
	if !strings.Contains(lines[0], `"_id":"CL-000001"`) || !strings.Contains(lines[0], `"_index":"test-index"`) {
		t.Errorf("action line = %s, want the Global ID as the document id", lines[0])
	}
	if !strings.Contains(lines[2], `"_id":"PR-000001"`) {
		t.Errorf("second action line = %s", lines[2])
	}
}

func TestBulkDeleteSendsDeleteActions(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(bulkResponse{Errors: false})
	})
	provider := newProvider(t, cluster)

	if err := provider.Delete(context.Background(), "CL-000001", " ", "PR-000001"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	body := cluster.requests[0].Body
	if strings.Count(body, `"delete"`) != 2 {
		t.Errorf("body = %s, want two delete actions and the blank id dropped", body)
	}
}

// Nothing is sent for an empty batch: a request that does nothing still costs
// a round trip and a log line.
func TestEmptyBatchesSendNothing(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) { t.Error("a request was sent") })
	provider := newProvider(t, cluster)
	if err := provider.Index(context.Background()); err != nil {
		t.Errorf("index: %v", err)
	}
	if err := provider.Delete(context.Background()); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := provider.Delete(context.Background(), "  "); err != nil {
		t.Errorf("delete of a blank id: %v", err)
	}
}

// A cluster reports per-item failures inside a 200. Treating the response as
// a success because the status was 200 would lose documents silently.
func TestPerItemBulkFailuresAreReported(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":true,"items":[{"index":{"status":400,"error":{"type":"mapper_parsing_exception","reason":"failed to parse field [title] with value [Northwind Trading]"}}}]}`))
	})
	provider := newProvider(t, cluster)

	err := provider.Index(context.Background(), document("CL-000001", TypeClient, "Northwind", "", "crm.client.read"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// The cluster's reason quotes the indexed document. That must not travel
	// into a platform error, which is read by people the document is not for.
	if strings.Contains(err.Error(), "Northwind") || strings.Contains(err.Error(), "failed to parse") {
		t.Errorf("the error carried the cluster's reason: %v", err)
	}
}

// A delete of an absent document comes back as a per-item 404. It is the one
// per-item failure that is not a failure.
func TestDeletingAnAbsentDocumentIsNotAFailure(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":true,"items":[{"delete":{"status":404,"error":{"type":"document_missing_exception","reason":"[CL-000009]: document missing"}}}]}`))
	})
	provider := newProvider(t, cluster)
	if err := provider.Delete(context.Background(), "CL-000009"); err != nil {
		t.Errorf("delete of an absent document: %v", err)
	}
}

func TestSearchNarrowsInTheQueryRatherThanInMemory(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hits":{"total":{"value":1},"hits":[{"_score":3.5,"_source":{"id":"CL-000001","type":"client","title":"Northwind","source":"espocrm","permissions":["crm.client.read"],"scope_type":"client","scope_id":"CL-000001"}}]}}`))
	})
	provider := newProvider(t, cluster)

	page, err := provider.Search(context.Background(), Query{
		Text: "northwind", Types: []string{TypeClient},
		Permissions: []string{"crm.client.read"}, Offset: 20, Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Hits) != 1 || page.Hits[0].ID != "CL-000001" || page.Hits[0].Score != 3.5 {
		t.Errorf("hits = %+v", page.Hits)
	}
	if page.Total != 1 {
		t.Errorf("total = %d", page.Total)
	}

	sent := cluster.requests[0]
	if sent.Path != "/test-index/_search" {
		t.Errorf("path = %s", sent.Path)
	}
	// A caller's page must not be assembled by fetching records they cannot
	// see and discarding them, so both filters belong in the query.
	for _, expected := range []string{`"permissions":["crm.client.read"]`, `"type":["client"]`, `"from":20`, `"operator":"and"`, `"title^2"`} {
		if !strings.Contains(sent.Body, expected) {
			t.Errorf("query body is missing %s:\n%s", expected, sent.Body)
		}
	}
}

// An administrator's query must not carry a permission filter, or it would
// intersect with their single "*" permission and match nothing.
func TestAnAdministratorsQueryCarriesNoPermissionFilter(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hits":{"total":{"value":0},"hits":[]}}`))
	})
	provider := newProvider(t, cluster)
	if _, err := provider.Search(context.Background(), Query{Text: "northwind", AllPermissions: true, Limit: 10}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if strings.Contains(cluster.requests[0].Body, `"permissions"`) {
		t.Errorf("an administrator's query was narrowed by permission:\n%s", cluster.requests[0].Body)
	}
}

func TestAnEmptyQueryNeverReachesTheCluster(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) { t.Error("a request was sent") })
	provider := newProvider(t, cluster)
	page, err := provider.Search(context.Background(), Query{Text: "   ", AllPermissions: true, Limit: 10})
	if err != nil || len(page.Hits) != 0 {
		t.Errorf("page = %+v, err = %v", page, err)
	}
}

// A failing cluster must be reported as a platform failure that names no
// status code, hostname or payload.
func TestClusterFailuresAreNormalized(t *testing.T) {
	cases := map[string]int{
		"unauthorized": http.StatusUnauthorized,
		"server error": http.StatusInternalServerError,
		"rate limited": http.StatusTooManyRequests,
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"reason":"no handler for uri [/test-index/_search] on host opensearch-internal-1"}}`))
			})
			provider := newProvider(t, cluster)
			_, err := provider.Search(context.Background(), Query{Text: "northwind", AllPermissions: true, Limit: 10})
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			message := err.Error()
			for _, leak := range []string{"opensearch-internal-1", "no handler", cluster.URL} {
				if strings.Contains(message, leak) {
					t.Errorf("the error leaked %q: %v", leak, err)
				}
			}
		})
	}
}

func TestACancelledSearchDoesNotHang(t *testing.T) {
	cluster := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	provider := newProvider(t, cluster)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Search(ctx, Query{Text: "northwind", AllPermissions: true, Limit: 10}); err == nil {
		t.Error("a cancelled search returned successfully")
	}
}
