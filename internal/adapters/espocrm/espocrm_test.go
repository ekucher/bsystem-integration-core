package espocrm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

func TestListAccountsAndHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/App/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			t.Fatalf("missing API key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1"}`))
	})
	mux.HandleFunc("/api/v1/Account", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("maxSize"); got != "25" {
			t.Fatalf("unexpected maxSize: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":1,"list":[{"id":"a1","name":"Acme"}]}`))
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
	accounts, err := client.ListAccounts(context.Background(), 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Name != "Acme" {
		t.Fatalf("unexpected accounts: %#v", accounts)
	}
}

func TestHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()
	client, err := New(server.URL, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListContacts(context.Background(), 10); err == nil {
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
	client, err := New(server.URL, "secret", time.Second)
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
	client, err := New(server.URL, "secret", time.Second)
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
