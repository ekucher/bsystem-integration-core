package main

// Rate limiting, by class of surface and by validated principal.
//
// The inventory this is built from is the route table in routes.go, which is
// already the authoritative list of what the platform serves. Four classes,
// because four things cost differently:
//
//   - human, the ordinary API a person's browser calls;
//   - service, the machine API, which is meant to be called in volume and is
//     bounded rather than throttled;
//   - AI, where one request costs a language model call and a fan-out of
//     authorized reads;
//   - search and the collection reads, where one request costs an upstream
//     query the platform does not control.
//
// The key is always a principal the platform has already authenticated. Never
// a header, a username or a client address: a limit keyed on something the
// caller controls is a limit the caller steps around by changing it, and one
// keyed on an address throttles everybody behind a shared egress together.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/ratelimit"
)

// limitClass names a bucket. It is a small closed set because it is a metric
// label: an unbounded one is a new time series per value.
type limitClass string

const (
	limitNone    limitClass = ""
	limitHuman   limitClass = "human"
	limitService limitClass = "service"
	limitAI      limitClass = "ai"
	limitSearch  limitClass = "search"
)

// limits holds one limiter per class.
type limits struct {
	byClass map[limitClass]*ratelimit.Limiter
}

// newLimits reads the policy from the environment.
//
// The defaults are generous on purpose. A limit is there to bound abuse, and a
// limit that a legitimate session meets is an outage the platform inflicted on
// itself. The AI class is the tight one, because one request there costs a
// model call and a fan-out of reads rather than a query.
func newLimits() *limits {
	return &limits{byClass: map[limitClass]*ratelimit.Limiter{
		limitHuman:   ratelimit.New(intEnv("RATE_LIMIT_HUMAN_PER_MINUTE", 300), intEnv("RATE_LIMIT_HUMAN_BURST", 60)),
		limitService: ratelimit.New(intEnv("RATE_LIMIT_SERVICE_PER_MINUTE", 1200), intEnv("RATE_LIMIT_SERVICE_BURST", 200)),
		limitAI:      ratelimit.New(intEnv("RATE_LIMIT_AI_PER_MINUTE", 20), intEnv("RATE_LIMIT_AI_BURST", 5)),
		limitSearch:  ratelimit.New(intEnv("RATE_LIMIT_SEARCH_PER_MINUTE", 120), intEnv("RATE_LIMIT_SEARCH_BURST", 30)),
	}}
}

// forClass returns the limiter for a class, and tolerates a limits that was
// never built.
//
// Not defensive programming for its own sake: the consequence of the nil is a
// panic on the first request the process serves, which is a crash loop rather
// than a missing limit. An unlimited route is the safer of the two failures
// and the one a test fixture should get.
func (l *limits) forClass(class limitClass) *ratelimit.Limiter {
	if l == nil {
		return nil
	}
	return l.byClass[class]
}

// classOf decides which bucket a route belongs to.
//
// Derived from the route rather than listed separately, so a route added to
// the table is limited by default instead of being unlimited until somebody
// remembers it. An explicit Class on the route overrides it.
func classOf(r route) limitClass {
	if r.Class != limitNone {
		return r.Class
	}
	switch r.Auth {
	case authHuman:
		return limitHuman
	case authService:
		return limitService
	default:
		// Operational endpoints. A limiter on /metrics breaks scraping during
		// exactly the incident the scrape exists for.
		return limitNone
	}
}

// limit refuses a request whose principal has spent its allowance.
//
// It runs after authentication, because the key is the authenticated
// principal, and before authorization, because refusing early is the cheaper
// half of the point.
func (a *app) limit(class limitClass, next http.Handler) http.Handler {
	limiter := a.limits.forClass(class)
	if !limiter.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := principalKey(r)
		if key == "" {
			// No principal resolved means authentication did not run or did
			// not succeed, and this middleware is not the place to decide
			// that. Passing through is safe: the layers around it refuse.
			next.ServeHTTP(w, r)
			return
		}
		allowed, wait := limiter.Allow(key)
		if allowed {
			rateLimitDecisions.Inc(string(class), "allowed")
			next.ServeHTTP(w, r)
			return
		}
		rateLimitDecisions.Inc(string(class), "rejected")
		// Seconds, rounded up, as RFC 9110 requires. Bounded by the limiter
		// itself, which never returns a wait longer than a full refill.
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error":      "too many requests",
			"code":       "rate_limited",
			"request_id": requestIDFrom(r.Context()),
		})
	})
}

// principalKey returns the identifier the limit is counted against.
//
// A Global ID: USR-* for a person, SVC-* for a machine. Nothing derived from a
// header, and nothing that identifies the human being behind it — the key
// never reaches a log line or a metric label.
func principalKey(r *http.Request) string {
	if principal, ok := r.Context().Value(principalContextKey).(authz.Principal); ok && principal.ID != "" {
		return principal.ID
	}
	if service, ok := r.Context().Value(serviceContextKey).(servicePrincipal); ok && service.ID != "" {
		return service.ID
	}
	return ""
}
