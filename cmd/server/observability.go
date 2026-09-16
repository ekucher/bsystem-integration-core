package main

import (
	gocontext "context"
	"net/http"
	"os"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/observability"
)

// sample runs a metric sample under a bounded context and releases it when
// the sample returns. A scrape must not be able to block on a dependency that
// has stopped answering: the point of scraping during an incident is to find
// out which dependency that is.
func sample[T any](fn func(ctx gocontext.Context) T) T {
	ctx, cancel := gocontext.WithTimeout(gocontext.Background(), 2*time.Second)
	defer cancel()
	return fn(ctx)
}

// metricsRegistry holds every series the platform exposes.
var metricsRegistry = observability.NewRegistry()

// httpMetrics are registered once, at startup, so that a route's series exist
// before the first request rather than appearing when it is first served.
var httpMetrics = observability.NewHTTPMetrics(metricsRegistry)

// registerPlatformMetrics adds the series that are sampled from the platform's
// own components at scrape time.
func (a *app) registerPlatformMetrics() {
	metricsRegistry.GaugeFunc(
		"bsystem_database_up",
		"PostgreSQL availability (1=up, 0=down).",
		nil,
		func() []observability.Sample {
			return sample(func(ctx gocontext.Context) []observability.Sample {
				if err := a.db.Ping(ctx); err != nil {
					return []observability.Sample{{Value: 0}}
				}
				return []observability.Sample{{Value: 1}}
			})
		},
	)

	// Pool saturation is the metric that explains a slow platform when every
	// individual query is fast: requests are waiting for a connection, not
	// for the database.
	metricsRegistry.GaugeFunc(
		"bsystem_database_pool_connections",
		"PostgreSQL pool connections by state.",
		[]string{"state"},
		func() []observability.Sample {
			stats := a.db.Stats()
			return []observability.Sample{
				{Labels: []string{"acquired"}, Value: float64(stats.Acquired)},
				{Labels: []string{"idle"}, Value: float64(stats.Idle)},
				{Labels: []string{"total"}, Value: float64(stats.Total)},
				{Labels: []string{"max"}, Value: float64(stats.Max)},
			}
		},
	)

	metricsRegistry.GaugeFunc(
		"bsystem_database_pool_acquires_total",
		"PostgreSQL pool acquisitions by outcome.",
		[]string{"outcome"},
		func() []observability.Sample {
			stats := a.db.Stats()
			return []observability.Sample{
				{Labels: []string{"total"}, Value: float64(stats.AcquireCount)},
				// An acquisition that had to wait for an empty pool is the
				// early warning that the pool is too small: every query is
				// fast and the platform is slow anyway.
				{Labels: []string{"empty"}, Value: float64(stats.EmptyAcquireCount)},
				{Labels: []string{"cancelled"}, Value: float64(stats.CanceledAcquire)},
			}
		},
	)

	metricsRegistry.GaugeFunc(
		"bsystem_nats_up",
		"NATS availability (1=connected, 0=not connected).",
		nil,
		func() []observability.Sample {
			if a.nc == nil || !a.nc.IsConnected() {
				return []observability.Sample{{Value: 0}}
			}
			return []observability.Sample{{Value: 1}}
		},
	)

	metricsRegistry.GaugeFunc(
		"bsystem_adapter_ready",
		"Adapter readiness (1=ready, 0=not ready).",
		[]string{"adapter"},
		func() []observability.Sample {
			return sample(func(ctx gocontext.Context) []observability.Sample {
				samples := []observability.Sample{}
				for id, health := range adapterRegistry.Health(ctx) {
					value := 0.0
					if health.Status == "ready" {
						value = 1
					}
					samples = append(samples, observability.Sample{Labels: []string{id}, Value: value})
				}
				return samples
			})
		},
	)

	// Exported as a gauge per state rather than as a numeric encoding, so an
	// alert can select state="open" instead of remembering which number means
	// open.
	metricsRegistry.GaugeFunc(
		"bsystem_adapter_circuit_state",
		"Adapter circuit breaker state (1=current).",
		[]string{"adapter", "state"},
		func() []observability.Sample {
			samples := []observability.Sample{}
			for adapter, current := range adapterRegistry.BreakerStates() {
				for _, state := range []string{"closed", "half_open", "open"} {
					value := 0.0
					if current == state {
						value = 1
					}
					samples = append(samples, observability.Sample{Labels: []string{adapter, state}, Value: value})
				}
			}
			return samples
		},
	)
}

// eventsPublished counts platform events by outcome, which is what
// distinguishes "nothing happened" from "everything failed to publish".
var eventsPublished = metricsRegistry.Counter(
	"bsystem_events_published_total",
	"Platform events published to NATS, by event and outcome.",
	"event", "outcome",
)

// notificationsRaised counts notifications the platform raised from events,
// by outcome. A mapped event that silently fails to become a notification is
// a message nobody receives, which looks identical to a quiet week.
var notificationsRaised = metricsRegistry.Counter(
	"bsystem_notifications_raised_total",
	"Notifications raised from platform events, by event and outcome.",
	"event", "outcome",
)

// searchIndexed counts index and delete operations by outcome. An indexing
// pipeline that has silently stopped looks exactly like one with nothing to
// do.
var searchIndexed = metricsRegistry.Counter(
	"bsystem_search_operations_total",
	"Search index operations, by operation and outcome.",
	"operation", "outcome",
)

var logger = observability.NewLogger(os.Stdout, observability.LevelFromEnv(os.Getenv("LOG_LEVEL")))

// observer wires request logging and metrics onto each route.
func (a *app) observer() *observability.Observer {
	return &observability.Observer{
		Logger:  logger,
		Metrics: httpMetrics,
		// Only the platform identifier is logged. An email or a name in a log
		// store is personal data the platform has no reason to accumulate,
		// and the Global ID resolves to the person when that is genuinely
		// needed.
		ActorFrom: func(r *http.Request) string {
			return principalFrom(r.Context()).ID
		},
		RequestIDFrom: func(r *http.Request) string {
			return requestIDFrom(r.Context())
		},
	}
}

// adapterRecorder turns upstream attempts into metrics.
//
// It lives here rather than in the transport so that the transport does not
// decide how the platform stores its observations, and so that adding a
// series does not mean touching adapter code.
type adapterRecorder struct {
	attempts *observability.CounterVec
	latency  *observability.HistogramVec
	rejected *observability.CounterVec
}

var upstream = &adapterRecorder{
	attempts: metricsRegistry.Counter(
		"bsystem_adapter_attempts_total",
		"Upstream attempts, by adapter, outcome and whether the attempt was a retry.",
		"adapter", "outcome", "retry",
	),
	latency: metricsRegistry.Histogram(
		"bsystem_adapter_attempt_duration_seconds",
		"Time for one upstream attempt, by adapter.",
		observability.DefaultDurationBuckets,
		"adapter",
	),
	rejected: metricsRegistry.Counter(
		"bsystem_adapter_circuit_rejected_total",
		"Calls the circuit breaker refused to attempt, by adapter.",
		"adapter",
	),
}

// Attempt records one upstream attempt.
//
// The outcome label is the failure kind, or "ok". Keeping the kinds distinct
// is what separates "the upstream is rate limiting us" from "the upstream is
// down" from "our credential is wrong" — three very different pages at three
// in the morning.
func (r *adapterRecorder) Attempt(adapter string, kind string, retry bool, seconds float64) {
	outcome := kind
	if outcome == "" {
		outcome = "ok"
	}
	wasRetry := "false"
	if retry {
		wasRetry = "true"
	}
	r.attempts.Inc(adapter, outcome, wasRetry)
	r.latency.Observe(seconds, adapter)
}

func (r *adapterRecorder) CircuitRejected(adapter string) {
	r.rejected.Inc(adapter)
}
