package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
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
	breakers := adapterRegistry.BreakerStates()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP bsystem_integration_database_up PostgreSQL availability (1=up, 0=down).")
	fmt.Fprintln(w, "# TYPE bsystem_integration_database_up gauge")
	fmt.Fprintf(w, "bsystem_integration_database_up %d\n", databaseUp)
	fmt.Fprintln(w, "# HELP bsystem_integration_nats_up NATS availability (1=up, 0=down).")
	fmt.Fprintln(w, "# TYPE bsystem_integration_nats_up gauge")
	fmt.Fprintf(w, "bsystem_integration_nats_up %d\n", natsUp)
	fmt.Fprintln(w, "# HELP bsystem_adapter_ready Adapter readiness (1=ready, 0=not ready).")
	fmt.Fprintln(w, "# TYPE bsystem_adapter_ready gauge")
	for _, id := range sortedKeys(adapterHealth) {
		ready := 0
		if adapterHealth[id].Status == "ready" {
			ready = 1
		}
		fmt.Fprintf(w, "bsystem_adapter_ready{adapter=%q} %d\n", id, ready)
	}

	// The circuit state is exported as one gauge per state rather than as a
	// numeric encoding, so a dashboard or alert can select a state by name
	// instead of remembering which number means open.
	fmt.Fprintln(w, "# HELP bsystem_adapter_circuit_state Adapter circuit breaker state (1=current).")
	fmt.Fprintln(w, "# TYPE bsystem_adapter_circuit_state gauge")
	for _, id := range sortedKeys(breakers) {
		for _, state := range []string{"closed", "half_open", "open"} {
			current := 0
			if breakers[id] == state {
				current = 1
			}
			fmt.Fprintf(w, "bsystem_adapter_circuit_state{adapter=%q,state=%q} %d\n", id, state, current)
		}
	}
}

// sortedKeys keeps the metrics output stable between scrapes. Prometheus does
// not require ordering, but a diffable exposition is far easier to debug.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
