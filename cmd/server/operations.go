package main

import (
	"context"
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
	// An open circuit is reported but does not make the platform unready: the
	// rest of the API keeps working while one upstream is being shed.
	for id, state := range adapterRegistry.BreakerStates() {
		if state != "closed" {
			checks["adapter:"+id] = "circuit_" + state
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": checks})
}

// metrics renders the platform's metrics registry.
//
// The series themselves are declared in observability.go and sampled from the
// components that own them, so this handler stays a renderer rather than
// becoming a second place where metrics are defined.
func (a *app) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metricsRegistry.Render(w)
}
