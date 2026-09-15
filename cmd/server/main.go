package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

type module struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type healthResponse struct {
	Status    string `json:"status"`
	Service   string `json:"service"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = time.Now().UTC().Format("20060102T150405.000000000Z07:00")
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Status:    "ok",
			Service:   "bsystem-integration-core",
			Version:   "0.1.0",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("GET /api/v1/modules", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []module{
			{ID: "crm", Name: "BSYSTEM CRM", Status: "planned"},
			{ID: "projects", Name: "BSYSTEM Projects", Status: "planned"},
			{ID: "qa", Name: "BSYSTEM QA", Status: "planned"},
			{ID: "development", Name: "BSYSTEM Development", Status: "planned"},
			{ID: "wiki", Name: "BSYSTEM Wiki", Status: "planned"},
			{ID: "operations", Name: "BSYSTEM Operations", Status: "planned"},
		})
	})

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           requestID(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("bsystem-integration-core listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}
