package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func (a *app) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"checks": map[string]string{"database": "error"},
		})
		return
	}
	checks := map[string]string{"database": "ok"}
	if a.nc == nil || !a.nc.IsConnected() {
		checks["nats"] = "degraded"
	} else {
		checks["nats"] = "ok"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": checks})
}

func (a *app) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	databaseUp := 1
	if err := a.db.Ping(ctx); err != nil {
		databaseUp = 0
	}
	natsUp := 0
	if a.nc != nil && a.nc.IsConnected() {
		natsUp = 1
	}

	adapterHealth := adapterRegistry.Health(r.Context())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP bsystem_integration_database_up PostgreSQL availability (1=up, 0=down).")
	fmt.Fprintln(w, "# TYPE bsystem_integration_database_up gauge")
	fmt.Fprintf(w, "bsystem_integration_database_up %d\n", databaseUp)
	fmt.Fprintln(w, "# HELP bsystem_integration_nats_up NATS availability (1=up, 0=down).")
	fmt.Fprintln(w, "# TYPE bsystem_integration_nats_up gauge")
	fmt.Fprintf(w, "bsystem_integration_nats_up %d\n", natsUp)
	fmt.Fprintln(w, "# HELP bsystem_adapter_ready Adapter readiness (1=ready, 0=not ready).")
	fmt.Fprintln(w, "# TYPE bsystem_adapter_ready gauge")
	for id, health := range adapterHealth {
		ready := 0
		if health.Status == "ready" {
			ready = 1
		}
		fmt.Fprintf(w, "bsystem_adapter_ready{adapter=%q} %d\n", id, ready)
	}
}

func registerOperationsRoutes(root *http.ServeMux, a *app) {
	root.HandleFunc("GET /readyz", a.readiness)
	root.HandleFunc("GET /metrics", a.metrics)
}
