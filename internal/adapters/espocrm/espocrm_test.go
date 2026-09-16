package espocrm

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

func TestListAccountsAndHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/App/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			t.Errorf("missing API key")
		}
		_, _ = w.Write([]byte(`{"user":{"id":"1"}}`))
	})
	mux.HandleFunc("/api/v1/Account", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("maxSize"); got != "25" {
			t.Errorf("maxSize = %s, want 25", got)
		}
		_, _ = w.Write([]byte(`{"total":1,"list":[{"id":"a1","name":"Acme"}]}`))
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
	accounts, info, err := client.ListAccounts(context.Background(), adapters.Page{Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Name != "Acme" {
		t.Fatalf("unexpected accounts: %#v", accounts)
	}
	if info.Total != 1 || info.NextCursor != "" {
		t.Fatalf("page info = %+v, want the collection exhausted", info)
	}
}

// The upstream reports the size of the whole collection while returning one
// window of it, and the adapter must turn that into a cursor the caller can
// walk without knowing the upstream's paging scheme.
func TestListAccountsPaginates(t *testing.T) {
	all := []string{"a1", "a2", "a3", "a4", "a5"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/Account", func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("maxSize"))
		end := min(offset+limit, len(all))
		items := ""
		for i := offset; i < end; i++ {
			if items != "" {
				items += ","
			}
			items += `{"id":"` + all[i] + `","name":"n"}`
		}
		_, _ = fmt.Fprintf(w, `{"total":%d,"list":[%s]}`, len(all), items)
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
		accounts, info, err := client.ListAccounts(context.Background(), page)
		if err != nil {
			t.Fatalf("list accounts: %v", err)
		}
		if info.Total != len(all) {
			t.Fatalf("total = %d, want %d: the total describes the collection, not the page", info.Total, len(all))
		}
		for _, account := range accounts {
			walked = append(walked, account.ID)
		}
		if info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
	}
	if len(walked) != len(all) {
		t.Fatalf("walked %v, want every item exactly once", walked)
	}
	for i, id := range all {
		if walked[i] != id {
			t.Fatalf("walked[%d] = %q, want %q", i, walked[i], id)
		}
	}
}

// The page size the platform will ask an upstream for is capped, so one
// request cannot pull an unbounded amount of upstream data.
func TestPageSizeIsBounded(t *testing.T) {
	tests := []struct {
		name        string
		limit       int
		wantMaxSize string
	}{
		{name: "unset uses the default", limit: 0, wantMaxSize: "50"},
		{name: "negative uses the default", limit: -1, wantMaxSize: "50"},
		{name: "in range is honoured", limit: 10, wantMaxSize: "10"},
		{name: "above the cap is clamped", limit: 100000, wantMaxSize: "200"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query().Get("maxSize")
				_, _ = w.Write([]byte(`{"total":0,"list":[]}`))
			}))
			defer server.Close()
			client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := client.ListAccounts(context.Background(), adapters.Page{Limit: test.limit}); err != nil {
				t.Fatalf("list accounts: %v", err)
			}
			if got != test.wantMaxSize {
				t.Fatalf("maxSize = %q, want %q", got, test.wantMaxSize)
			}
		})
	}
}

// A cursor the platform did not issue cannot be interpreted, and guessing at
// it would silently return the wrong window.
func TestForgedCursorIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total":0,"list":[]}`))
	}))
	defer server.Close()
	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.ListAccounts(context.Background(), adapters.Page{Cursor: "not-a-cursor"}); !errors.Is(err, adapters.ErrInvalidCursor) {
		t.Fatalf("error = %v, want adapters.ErrInvalidCursor", err)
	}
}

func TestHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()
	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.ListContacts(context.Background(), adapters.Page{Limit: 10}); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func newDetailClient(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/Account/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "acc-northwind" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"acc-northwind","name":"Northwind Trading","website":"https://northwind.example.invalid"}`))
	})
	mux.HandleFunc("/api/v1/Contact/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "ct-anna" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"ct-anna","name":"Anna Kovalenko","accountId":"acc-northwind"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return client
}

func TestGetAccountAndContact(t *testing.T) {
	client := newDetailClient(t)

	account, err := client.GetAccount(context.Background(), "acc-northwind")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if account.Name != "Northwind Trading" || account.Website == "" {
		t.Fatalf("unexpected account: %#v", account)
	}

	contact, err := client.GetContact(context.Background(), "ct-anna")
	if err != nil {
		t.Fatalf("get contact: %v", err)
	}
	if contact.Name != "Anna Kovalenko" || contact.AccountID != "acc-northwind" {
		t.Fatalf("unexpected contact: %#v", contact)
	}
}

// A missing upstream record must be distinguishable from an upstream failure,
// so the platform can answer 404 instead of reporting the source as down.
func TestDetailReadsReportMissingRecordsAsNotFound(t *testing.T) {
	client := newDetailClient(t)
	tests := []struct {
		name string
		read func() error
	}{
		{name: "unknown account", read: func() error { _, err := client.GetAccount(context.Background(), "acc-missing"); return err }},
		{name: "unknown contact", read: func() error { _, err := client.GetContact(context.Background(), "ct-missing"); return err }},
		{name: "empty account id", read: func() error { _, err := client.GetAccount(context.Background(), " "); return err }},
		{name: "empty contact id", read: func() error { _, err := client.GetContact(context.Background(), ""); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.read(); !errors.Is(err, adapters.ErrNotFound) {
				t.Fatalf("error = %v, want adapters.ErrNotFound", err)
			}
		})
	}
}

// An upstream that is genuinely broken must not be mistaken for a missing
// record, or a real outage would surface as an empty result.
func TestUpstreamFailureIsNotReportedAsNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/Account/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(adapters.Config{BaseURL: server.URL, APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	_, err = client.GetAccount(context.Background(), "acc-northwind")
	if err == nil {
		t.Fatal("an upstream 500 must surface as an error")
	}
	if errors.Is(err, adapters.ErrNotFound) {
		t.Fatalf("an upstream 500 must not be reported as a missing record: %v", err)
	}
}
