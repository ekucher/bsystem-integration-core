package espocrm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
