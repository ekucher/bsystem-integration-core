package observability

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// HTTPMetrics holds the series describing served requests.
type HTTPMetrics struct {
	Requests *CounterVec
	Latency  *HistogramVec
	InFlight *GaugeVec
}

// NewHTTPMetrics registers the HTTP series on a registry.
//
// Every series is labelled by the route's registered pattern rather than by
// the request path. A path label would add a new time series for every Global
// ID the platform has ever been asked about, which makes the metric expensive
// to store and useless to group by — the failure mode usually discovered when
// a monitoring bill arrives.
func NewHTTPMetrics(registry *Registry) *HTTPMetrics {
	return &HTTPMetrics{
		Requests: registry.Counter(
			"bsystem_http_requests_total",
			"HTTP requests handled, by route, method and status class.",
			"route", "method", "status", "status_class",
		),
		Latency: registry.Histogram(
			"bsystem_http_request_duration_seconds",
			"Time to handle an HTTP request, by route and method.",
			DefaultDurationBuckets,
			"route", "method",
		),
		InFlight: registry.Gauge(
			"bsystem_http_requests_in_flight",
			"Requests currently being handled, by route.",
			"route",
		),
	}
}

// responseRecorder captures the status so it can be measured and logged.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if !r.written {
		r.status = status
		r.written = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(payload []byte) (int, error) {
	// A handler that writes without calling WriteHeader has implicitly sent
	// 200; recording that keeps the status label accurate.
	if !r.written {
		r.status = http.StatusOK
		r.written = true
	}
	return r.ResponseWriter.Write(payload)
}

// Observe wraps a handler with request logging and metrics for one route.
//
// The route pattern is passed in rather than read from the request, because
// the pattern is known where the route is declared and reading it back would
// mean depending on the router having already matched.
type Observer struct {
	Logger  *slog.Logger
	Metrics *HTTPMetrics
	// ActorFrom extracts the caller's Global ID for the log line, or an empty
	// string when the request is unauthenticated. Only the platform
	// identifier is logged: an email or a name in a log store is personal
	// data the platform has no reason to accumulate.
	ActorFrom func(*http.Request) string
	// RequestIDFrom extracts the correlation id.
	RequestIDFrom func(*http.Request) string
}

// Observe returns next wrapped with logging and metrics for the given route.
func (o *Observer) Observe(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Adjusted rather than set: overlapping requests on one route must
		// each be counted, and the first to finish must not zero the others.
		o.Metrics.InFlight.Add(1, route)

		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		elapsed := time.Since(start)
		status := strconv.Itoa(recorder.status)

		o.Metrics.InFlight.Add(-1, route)
		o.Metrics.Requests.Inc(route, r.Method, status, statusClass(recorder.status))
		o.Metrics.Latency.Observe(elapsed.Seconds(), route, r.Method)

		attrs := []slog.Attr{
			slog.String("route", route),
			slog.String("method", r.Method),
			slog.Int("status", recorder.status),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
		}
		if o.RequestIDFrom != nil {
			if id := o.RequestIDFrom(r); id != "" {
				attrs = append(attrs, slog.String("request_id", id))
			}
		}
		if o.ActorFrom != nil {
			if actor := o.ActorFrom(r); actor != "" {
				attrs = append(attrs, slog.String("actor", actor))
			}
		}

		// A server error is the platform's own fault and is logged as an
		// error; a client error is the caller's and is not, or a burst of
		// wrong passwords would read as an outage.
		level := slog.LevelInfo
		if recorder.status >= 500 {
			level = slog.LevelError
		} else if recorder.status >= 400 {
			level = slog.LevelWarn
		}
		o.Logger.LogAttrs(r.Context(), level, "http request", attrs...)
	})
}

// statusClass buckets a status into the family used for alerting, so a rule
// can match "any server error" without enumerating codes.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}
