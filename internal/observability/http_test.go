package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newObserver(t *testing.T) (*Observer, *Registry, *bytes.Buffer) {
	t.Helper()
	buffer := &bytes.Buffer{}
	registry := NewRegistry()
	return &Observer{
		Logger:        NewLogger(buffer, slog.LevelInfo),
		Metrics:       NewHTTPMetrics(registry),
		RequestIDFrom: func(r *http.Request) string { return r.Header.Get("X-Request-ID") },
		ActorFrom:     func(r *http.Request) string { return r.Header.Get("X-Test-Actor") },
	}, registry, buffer
}

func call(observer *Observer, route string, handler http.HandlerFunc, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/clients/CL-000001", nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	observer.Observe(route, handler).ServeHTTP(recorder, request)
	return recorder
}

func TestRequestIsLoggedWithItsCorrelationId(t *testing.T) {
	observer, _, buffer := newObserver(t)
	call(observer, "GET /api/v1/clients/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, map[string]string{"X-Request-ID": "corr-1", "X-Test-Actor": "USR-000001"})

	var entry map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buffer.String())
	}
	for key, want := range map[string]any{
		"route":      "GET /api/v1/clients/{id}",
		"method":     "GET",
		"status":     float64(200),
		"request_id": "corr-1",
		"actor":      "USR-000001",
	} {
		if entry[key] != want {
			t.Errorf("%s = %v, want %v", key, entry[key], want)
		}
	}
	if _, present := entry["duration_ms"]; !present {
		t.Error("the log line must record how long the request took")
	}
}

// A path label would add a time series for every Global ID the platform has
// ever been asked about. The route pattern keeps the label set bounded.
func TestMetricsAreLabelledByRouteNotPath(t *testing.T) {
	observer, registry, _ := newObserver(t)
	handler := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

	for _, id := range []string{"CL-000001", "CL-000002", "CL-000003"} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/clients/"+id, nil)
		observer.Observe("GET /api/v1/clients/{id}", http.HandlerFunc(handler)).ServeHTTP(httptest.NewRecorder(), request)
	}

	var output strings.Builder
	registry.Render(&output)
	if strings.Contains(output.String(), "CL-000001") {
		t.Fatalf("a resource identifier reached a metric label:\n%s", output.String())
	}
	if got := observer.Metrics.Requests.Value("GET /api/v1/clients/{id}", "GET", "200", "2xx"); got != 3 {
		t.Fatalf("requests = %v, want 3 on one series", got)
	}
}

func TestStatusClassIsRecorded(t *testing.T) {
	tests := []struct {
		status int
		class  string
		level  string
	}{
		{status: 200, class: "2xx", level: "INFO"},
		{status: 403, class: "4xx", level: "WARN"},
		{status: 404, class: "4xx", level: "WARN"},
		{status: 502, class: "5xx", level: "ERROR"},
		{status: 503, class: "5xx", level: "ERROR"},
	}
	for _, test := range tests {
		t.Run(test.class, func(t *testing.T) {
			observer, _, buffer := newObserver(t)
			call(observer, "GET /api/v1/clients", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}, nil)

			if got := observer.Metrics.Requests.Value("GET /api/v1/clients", "GET", itoa(test.status), test.class); got != 1 {
				t.Fatalf("no series for status class %s", test.class)
			}
			// A client error is the caller's fault, not the platform's: a
			// burst of wrong tokens must not read as an outage.
			if !strings.Contains(buffer.String(), `"level":"`+test.level+`"`) {
				t.Fatalf("level for %d = %s, want %s", test.status, buffer.String(), test.level)
			}
		})
	}
}

// A handler that writes a body without calling WriteHeader has implicitly
// sent 200, and the metric must say so rather than recording nothing.
func TestImplicitOkIsRecorded(t *testing.T) {
	observer, _, _ := newObserver(t)
	call(observer, "GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"USR-000001"}`))
	}, nil)

	if got := observer.Metrics.Requests.Value("GET /api/v1/me", "GET", "200", "2xx"); got != 1 {
		t.Fatalf("an implicit 200 was not recorded: %v", got)
	}
}

func TestInFlightReturnsToZero(t *testing.T) {
	observer, _, _ := newObserver(t)
	release := make(chan struct{})
	var started sync.WaitGroup
	var finished sync.WaitGroup

	for worker := 0; worker < 4; worker++ {
		started.Add(1)
		finished.Add(1)
		go func() {
			defer finished.Done()
			call(observer, "GET /api/v1/clients", func(w http.ResponseWriter, _ *http.Request) {
				started.Done()
				<-release
				w.WriteHeader(http.StatusOK)
			}, nil)
		}()
	}

	started.Wait()
	if got := observer.Metrics.InFlight.Value("GET /api/v1/clients"); got != 4 {
		t.Fatalf("in flight = %v, want 4 while all four are running", got)
	}
	close(release)
	finished.Wait()

	if got := observer.Metrics.InFlight.Value("GET /api/v1/clients"); got != 0 {
		t.Fatalf("in flight = %v, want 0 once every request finished", got)
	}
}

// The middleware must not swallow the handler's response.
func TestResponseReachesTheClient(t *testing.T) {
	observer, _, _ := newObserver(t)
	recorder := call(observer, "GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("body"))
	}, nil)

	if recorder.Code != http.StatusTeapot || recorder.Body.String() != "body" {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

// Nothing the middleware logs may carry a credential, even when one arrives
// in a header it can see.
func TestRequestLoggingDoesNotCarryCredentials(t *testing.T) {
	observer, _, buffer := newObserver(t)
	call(observer, "GET /api/v1/clients", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, map[string]string{"Authorization": "Bearer super-secret", "Cookie": "session=super-secret"})

	if strings.Contains(buffer.String(), "super-secret") {
		t.Fatalf("a credential reached the log: %s", buffer.String())
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}
