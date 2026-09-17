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

	// A counter, not a gauge: the pool's tallies only ever increase within a
	// process, the name says `_total`, and the dashboard takes rate() of it.
	metricsRegistry.CounterFunc(
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

	// The queue itself, sampled. The counter above says what delivery is
	// doing; this says whether it is keeping up. A queued count that climbs
	// while attempts fail is a broker outage being survived; a failed count
	// above zero is an event that will never be delivered without somebody
	// looking.
	metricsRegistry.GaugeFunc(
		"bsystem_event_outbox_events",
		"Durable events in the outbox, by state.",
		[]string{"state"},
		func() []observability.Sample {
			return sample(func(ctx gocontext.Context) []observability.Sample {
				counts, err := a.db.OutboxState(ctx)
				if err != nil {
					// A database that cannot be read cannot report a queue
					// depth. Emitting zeroes would draw an empty queue during
					// exactly the outage where the queue matters most.
					return nil
				}
				return []observability.Sample{
					{Labels: []string{"queued"}, Value: float64(counts.Queued)},
					{Labels: []string{"retrying"}, Value: float64(counts.Retried)},
					{Labels: []string{"failed"}, Value: float64(counts.Failed)},
				}
			})
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

// auditWrites counts audit records by action and outcome.
//
// The same reasoning as eventsPublished, applied to the record that is least
// recoverable when it is missing. A failed audit write was only a log line, so
// a platform whose audit trail had silently stopped being written looked
// exactly like a quiet one on every dashboard.
//
// The asymmetry matters: an event that fails to publish can be re-derived from
// the state that produced it, and a notification can be raised again. An audit
// record that was never written cannot be reconstructed from anything, because
// the whole point of it is to record that something happened to a system that
// keeps no other trace of who asked.
//
// The action is a label and the resource is not: the action is a small closed
// set, while a resource id is unbounded and would make a new time series per
// record.
var auditWrites = metricsRegistry.Counter(
	"bsystem_audit_writes_total",
	"Audit records written, by action and outcome.",
	"action", "outcome",
)

// outboxAttempts counts durable-event delivery attempts by subject and
// outcome.
//
// The three outcomes are not decoration. "delivered" is a broker
// acknowledgement. "failed" is an attempt that will be retried, and a rate of
// it that does not fall is a broker problem rather than a platform one.
// "ack_not_recorded" is the one that needs a person: the broker has the event
// and the platform could not write that down, so the event will be published
// again and the consumer's own deduplication is what keeps it correct.
//
// The subject is a label and the event id is not: subjects are a small closed
// set, an event id is one time series per event.
var outboxAttempts = metricsRegistry.Counter(
	"bsystem_event_outbox_attempts_total",
	"Durable event delivery attempts, by subject and outcome.",
	"subject", "outcome",
)

// rateLimitDecisions counts requests allowed and rejected by class.
//
// The labels are the class and the outcome, and nothing else. A limit is
// counted against a principal, and the principal is exactly what must not
// appear here: a Global ID is one time series per person or per machine
// identity, and a username, an email or a client address would put who is
// being throttled into a metrics store that is read far more widely than the
// audit trail.
//
// Which principal is being limited is answerable from the audit trail and the
// logs. How much of each surface is being refused is answerable only from
// here, and it is the question an operator has during an incident.
var rateLimitDecisions = metricsRegistry.Counter(
	"bsystem_rate_limit_decisions_total",
	"Requests allowed and rejected by the rate limiter, by class.",
	"class", "outcome",
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

// operationsReported counts reports by event and severity. A reporter that
// has gone quiet looks identical to infrastructure with nothing to report,
// and only the count distinguishes them.
var operationsReported = metricsRegistry.Counter(
	"bsystem_operations_events_total",
	"Operations events reported, by event and severity.",
	"event", "severity",
)

// aiRequests counts gateway calls by provider and outcome. A refusal is as
// important to see as an answer: a rising refused_credential count means
// somebody is repeatedly asking the platform to send secrets to a model.
var aiRequests = metricsRegistry.Counter(
	"bsystem_ai_requests_total",
	"AI gateway requests, by provider and result.",
	"provider", "result",
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
