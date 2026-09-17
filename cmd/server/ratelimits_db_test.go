package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/ratelimit"
)

// tightlyLimitedApp is the fixture with one class turned down far enough to
// reach in a test.
func tightlyLimitedApp(t *testing.T, principals map[string]map[string]any, class limitClass, perMinute, burst int) (*app, http.Handler) {
	t.Helper()
	application, _ := integrationApp(t, principals)
	application.limits.byClass[class] = ratelimit.New(perMinute, burst)
	return application, application.handler()
}

// The property that makes a per-principal limit a limit rather than a shared
// queue: one caller spending their allowance costs them and nobody else.
func TestOneNoisyPrincipalDoesNotThrottleAnother(t *testing.T) {
	_, handler := tightlyLimitedApp(t, map[string]map[string]any{
		"noisy": principal("rate-noisy", "BSYSTEM-Admins"),
		"quiet": principal("rate-quiet", "BSYSTEM-Admins"),
	}, limitHuman, 60, 5)

	// Both sign in first, so the allowance being spent below is the API's and
	// not the sign-in's.
	for _, token := range []string{"noisy", "quiet"} {
		if recorder := call(t, handler, http.MethodGet, "/api/v1/me", token); recorder.Code != http.StatusOK {
			t.Fatalf("sign in as %s: status = %d", token, recorder.Code)
		}
	}

	refused := 0
	for i := 0; i < 30; i++ {
		if recorder := call(t, handler, http.MethodGet, "/api/v1/me", "noisy"); recorder.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("the noisy principal was never refused, so the rest of this test proves nothing")
	}

	// The quiet principal is untouched.
	for i := 0; i < 4; i++ {
		recorder := call(t, handler, http.MethodGet, "/api/v1/me", "quiet")
		if recorder.Code != http.StatusOK {
			t.Fatalf("a quiet principal got %d on request %d while another was being throttled (body: %s)", recorder.Code, i+1, recorder.Body.String())
		}
	}
}

// What a refusal has to tell the caller, and what it must not.
func TestARefusalIsNormalisedAndCarriesABoundedRetryAfter(t *testing.T) {
	_, handler := tightlyLimitedApp(t, map[string]map[string]any{
		"admin": principal("rate-shape", "BSYSTEM-Admins"),
	}, limitHuman, 60, 1)

	var refusal *http.Response
	var body string
	var retryAfter string
	for i := 0; i < 10; i++ {
		recorder := call(t, handler, http.MethodGet, "/api/v1/me", "admin")
		if recorder.Code == http.StatusTooManyRequests {
			refusal = recorder.Result()
			body = recorder.Body.String()
			retryAfter = recorder.Header().Get("Retry-After")
			break
		}
	}
	if refusal == nil {
		t.Fatal("a burst of one was never exceeded in ten requests")
	}

	var failure map[string]string
	if err := json.Unmarshal([]byte(body), &failure); err != nil {
		t.Fatalf("the refusal is not the platform's normalized error shape: %v (body: %s)", err, body)
	}
	if failure["code"] != "rate_limited" {
		t.Errorf("code = %q, want rate_limited", failure["code"])
	}
	if failure["request_id"] == "" {
		t.Error("the refusal carries no request id; a caller reporting it has nothing to quote")
	}
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After = %q, which is not a number of seconds", retryAfter)
	}
	if seconds < 1 {
		t.Error("Retry-After is zero; a caller told to wait for nothing retries immediately and is refused again")
	}
	if seconds > 120 {
		t.Errorf("Retry-After = %ds: a bound that long is a caller giving up rather than backing off", seconds)
	}
	// A refusal must not describe the principal it refused, the policy or the
	// platform. It is returned to whoever sent the request.
	for _, secret := range []string{"USR-", "rate-shape", "bucket", "token"} {
		if strings.Contains(body, secret) {
			t.Errorf("the refusal names %q: %s", secret, body)
		}
	}
}

// The operational endpoints are not limited. A limiter on /metrics stops the
// scrape during exactly the incident the scrape exists for, and a limiter on
// /readyz makes an orchestrator kill a healthy container.
func TestTheOperationalEndpointsAreNotLimited(t *testing.T) {
	_, handler := tightlyLimitedApp(t, nil, limitHuman, 60, 1)
	for _, path := range []string{"/health", "/readyz", "/metrics"} {
		for i := 0; i < 20; i++ {
			if recorder := call(t, handler, http.MethodGet, path, ""); recorder.Code == http.StatusTooManyRequests {
				t.Fatalf("%s was rate limited on request %d", path, i+1)
			}
		}
	}
}

// A route added to the table must be limited by default rather than unlimited
// until somebody remembers it.
func TestEveryAuthenticatedRouteFallsIntoAClass(t *testing.T) {
	for _, r := range routes() {
		class := classOf(r)
		switch r.Auth {
		case authNone:
			if class != limitNone {
				t.Errorf("%s is unauthenticated and was put in class %q", r.Pattern(), class)
			}
		default:
			if class == limitNone {
				t.Errorf("%s is authenticated and falls into no rate-limit class", r.Pattern())
			}
		}
	}
}

// The metric must be able to describe how much of each surface is being
// refused, and must not be able to describe who.
func TestTheRateLimitMetricNamesNoPrincipal(t *testing.T) {
	_, handler := tightlyLimitedApp(t, map[string]map[string]any{
		"admin": principal("rate-metric", "BSYSTEM-Admins"),
	}, limitHuman, 60, 1)
	for i := 0; i < 5; i++ {
		call(t, handler, http.MethodGet, "/api/v1/me", "admin")
	}

	metrics := call(t, handler, http.MethodGet, "/metrics", "").Body.String()
	if !strings.Contains(metrics, `bsystem_rate_limit_decisions_total{class="human",outcome="rejected"}`) {
		t.Error("rejections are not counted; an operator cannot tell a throttled surface from a quiet one")
	}
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, "bsystem_rate_limit_decisions_total") {
			continue
		}
		for _, leak := range []string{"USR-", "SVC-", "rate-metric", "@", "127.0.0.1"} {
			if strings.Contains(line, leak) {
				t.Errorf("the metric identifies who is being limited: %s", line)
			}
		}
	}
}
