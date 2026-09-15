package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

const testCredential = "test-upstream-api-key"

// recordedSleep captures backoff intervals instead of waiting them out, so the
// retry tests assert on the policy rather than on the clock.
type recordedSleep struct{ delays []time.Duration }

func (r *recordedSleep) sleep(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.delays = append(r.delays, delay)
	return nil
}

func newClient(t *testing.T, baseURL string, adjust func(*Options)) (*Client, *recordedSleep) {
	t.Helper()
	recorder := &recordedSleep{}
	options := Options{
		Adapter: "testupstream",
		BaseURL: baseURL,
		Timeout: 2 * time.Second,
		Retry:   RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: time.Second, sleep: recorder.sleep, jitter: func(d time.Duration) time.Duration { return d }},
		Breaker: BreakerSettings{FailureThreshold: 0},
	}
	if adjust != nil {
		adjust(&options)
	}
	client, err := New(options)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return client, recorder
}

func TestNewValidatesOptions(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		wantErr bool
	}{
		{name: "valid", options: Options{Adapter: "a", BaseURL: "https://upstream.example.invalid"}},
		{name: "trailing slash", options: Options{Adapter: "a", BaseURL: "https://upstream.example.invalid/"}},
		{name: "missing adapter", options: Options{BaseURL: "https://upstream.example.invalid"}, wantErr: true},
		{name: "missing base url", options: Options{Adapter: "a"}, wantErr: true},
		{name: "scheme-less base url", options: Options{Adapter: "a", BaseURL: "upstream.example.invalid"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.options)
			if test.wantErr != (err != nil) {
				t.Fatalf("New() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestDoDecodesAndSendsCredentials(t *testing.T) {
	var gotPath, gotQuery, gotAccept, gotCredential, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAccept, gotCredential = r.Header.Get("Accept"), r.Header.Get("X-Api-Key")
		buffer := make([]byte, 256)
		n, _ := r.Body.Read(buffer)
		gotBody = string(buffer[:n])
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	client, _ := newClient(t, server.URL, nil)
	var out struct {
		Value string `json:"value"`
	}
	err := client.Do(context.Background(), Request{
		Method: http.MethodPost,
		Path:   "/api/thing",
		Query:  map[string][]string{"limit": {"5"}},
		Body:   map[string]any{"id": "x"},
		Header: http.Header{"X-Api-Key": {testCredential}},
	}, &out)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.Value != "ok" {
		t.Fatalf("value = %q, want ok", out.Value)
	}
	if gotPath != "/api/thing" || gotQuery != "limit=5" {
		t.Fatalf("target = %s?%s", gotPath, gotQuery)
	}
	if gotAccept != "application/json" || gotCredential != testCredential {
		t.Fatalf("headers = accept:%q credential-present:%v", gotAccept, gotCredential != "")
	}
	if !strings.Contains(gotBody, `"id":"x"`) {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestStatusCodesMapToKinds(t *testing.T) {
	tests := []struct {
		status int
		want   Kind
	}{
		{status: 401, want: KindUnauthorized},
		{status: 403, want: KindUnauthorized},
		{status: 404, want: KindNotFound},
		{status: 408, want: KindTimeout},
		{status: 429, want: KindRateLimited},
		{status: 500, want: KindUnavailable},
		{status: 502, want: KindUnavailable},
		{status: 503, want: KindUnavailable},
		{status: 400, want: KindUnavailable},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			client, _ := newClient(t, server.URL, func(o *Options) { o.Retry.MaxAttempts = 1 })
			err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"}, nil)
			if KindOf(err) != test.want {
				t.Fatalf("kind = %q, want %q (err: %v)", KindOf(err), test.want, err)
			}
		})
	}
}

// A missing upstream record must satisfy the platform-wide sentinel, so a
// handler can answer 404 without knowing which adapter it is talking to.
func TestNotFoundMatchesThePlatformSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer server.Close()
	client, _ := newClient(t, server.URL, nil)
	err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, nil)
	if !errors.Is(err, adapters.ErrNotFound) {
		t.Fatalf("error = %v, want adapters.ErrNotFound", err)
	}
}

// An adapter error reaches logs and, normalized, the platform boundary. It
// must never carry a credential, a URL, an internal hostname or the
// upstream's own error body.
func TestErrorsDoNotLeakCredentialsOrTopology(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"connection to db-primary.internal failed for user svc_bsystem"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	client, _ := newClient(t, server.URL, nil)
	err := client.Do(context.Background(), Request{
		Method: http.MethodGet, Path: "/secret-path", Idempotent: true,
		Query:  map[string][]string{"api_key": {testCredential}},
		Header: http.Header{"Authorization": {"Bearer " + testCredential}},
	}, nil)
	if err == nil {
		t.Fatal("an upstream 500 must surface as an error")
	}
	message := err.Error()
	for _, forbidden := range []string{testCredential, "db-primary.internal", "svc_bsystem", "secret-path", server.URL, "Bearer", "api_key"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("error message leaked %q: %s", forbidden, message)
		}
	}
	if !strings.Contains(message, "testupstream") || !strings.Contains(message, "500") {
		t.Fatalf("error message must still identify the adapter and status: %s", message)
	}
}

// --- Retries ---------------------------------------------------------------

func TestRetriesOnlyRetryableFailures(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		wantAttempts int32
	}{
		{name: "rate limited", status: 429, wantAttempts: 3},
		{name: "server error", status: 500, wantAttempts: 3},
		{name: "gateway error", status: 502, wantAttempts: 3},
		{name: "unauthorized", status: 401, wantAttempts: 1},
		{name: "forbidden", status: 403, wantAttempts: 1},
		{name: "not found", status: 404, wantAttempts: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			client, _ := newClient(t, server.URL, nil)
			_ = client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, nil)
			if got := atomic.LoadInt32(&attempts); got != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", got, test.wantAttempts)
			}
		})
	}
}

// A request that is not marked idempotent is attempted once, whatever the
// failure: retrying it risks duplicating an effect the platform cannot undo.
func TestNonIdempotentRequestsAreNeverRetried(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, _ := newClient(t, server.URL, nil)
	_ = client.Do(context.Background(), Request{Method: http.MethodPost, Path: "/x"}, nil)
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestRetrySucceedsAfterATransientFailure(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"value":"recovered"}`))
	}))
	defer server.Close()

	client, recorder := newClient(t, server.URL, nil)
	var out struct {
		Value string `json:"value"`
	}
	if err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.Value != "recovered" {
		t.Fatalf("value = %q", out.Value)
	}
	// Backoff doubles between attempts.
	if len(recorder.delays) != 2 || recorder.delays[1] != 2*recorder.delays[0] {
		t.Fatalf("backoff = %v, want two intervals with the second twice the first", recorder.delays)
	}
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	client, _ := newClient(t, "https://upstream.example.invalid", func(o *Options) {
		o.Retry = RetryPolicy{MaxAttempts: 10, BaseDelay: 100 * time.Millisecond, MaxDelay: 500 * time.Millisecond, sleep: sleepContext}
	})
	for attempt := 1; attempt <= 8; attempt++ {
		delay := client.backoff(attempt, 0)
		if delay <= 0 || delay > 500*time.Millisecond {
			t.Fatalf("attempt %d delay = %s, want within (0, 500ms]", attempt, delay)
		}
	}

	// Full jitter must actually vary, or a synchronized fleet retries in
	// lockstep and re-creates the burst that caused the failure.
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[client.backoff(4, 0)] = true
	}
	if len(seen) < 5 {
		t.Fatalf("jitter produced only %d distinct delays across 50 samples", len(seen))
	}
}

func TestRetryAfterIsHonouredAndBounded(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		wantDelay time.Duration
		wantExact bool
	}{
		{name: "seconds within the cap", header: "1", wantDelay: time.Second, wantExact: true},
		{name: "seconds beyond the cap", header: "3600", wantDelay: 2 * time.Second, wantExact: true},
		{name: "negative is ignored", header: "-5"},
		{name: "unparseable is ignored", header: "soon"},
		{name: "a past date is ignored", header: "Mon, 02 Jan 2006 15:04:05 GMT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", test.header)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()

			client, recorder := newClient(t, server.URL, func(o *Options) {
				o.Retry = RetryPolicy{MaxAttempts: 2, BaseDelay: 10 * time.Millisecond, MaxDelay: 2 * time.Second, jitter: func(d time.Duration) time.Duration { return d }}
			})
			recorder = &recordedSleep{}
			client.retry.sleep = recorder.sleep

			_ = client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, nil)
			if len(recorder.delays) != 1 {
				t.Fatalf("delays = %v, want one", recorder.delays)
			}
			if test.wantExact {
				if recorder.delays[0] != test.wantDelay {
					t.Fatalf("delay = %s, want %s", recorder.delays[0], test.wantDelay)
				}
				return
			}
			// An ignored header falls back to the ordinary backoff.
			if recorder.delays[0] != 10*time.Millisecond {
				t.Fatalf("delay = %s, want the base backoff", recorder.delays[0])
			}
		})
	}
}

// --- Cancellation and bounds -----------------------------------------------

// A cancelled caller must stop the work immediately, and must not be reported
// as an upstream failure: the upstream did nothing wrong.
func TestCancellationStopsRetriesAndIsNotAnUpstreamFault(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client, _ := newClient(t, server.URL, func(o *Options) {
		o.Retry = RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: time.Second,
			sleep:  func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() },
			jitter: func(d time.Duration) time.Duration { return d }}
	})

	err := client.Do(ctx, Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, nil)
	if KindOf(err) != KindCanceled {
		t.Fatalf("kind = %q, want %q (err: %v)", KindOf(err), KindCanceled, err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1: cancellation must stop the retry loop", got)
	}
}

func TestTimeoutIsClassifiedAsSuch(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() { close(release); server.Close() }()

	client, _ := newClient(t, server.URL, func(o *Options) {
		o.Timeout = 50 * time.Millisecond
		o.Retry.MaxAttempts = 1
	})
	err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"}, nil)
	if KindOf(err) != KindTimeout {
		t.Fatalf("kind = %q, want %q (err: %v)", KindOf(err), KindTimeout, err)
	}
}

// A hostile or broken upstream must not be able to exhaust the platform's
// memory with an unbounded response.
func TestResponseBodiesAreBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"` + strings.Repeat("x", 4096) + `"}`))
	}))
	defer server.Close()

	client, _ := newClient(t, server.URL, func(o *Options) { o.MaxBodyBytes = 128 })
	var out map[string]any
	err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"}, &out)
	if KindOf(err) != KindTooLarge {
		t.Fatalf("kind = %q, want %q (err: %v)", KindOf(err), KindTooLarge, err)
	}
	// An oversized response is not retried: it will be oversized again.
	if errors.Is(err, ErrRetryable) {
		t.Fatal("an oversized response must not be retried")
	}
}

func TestUnreadableBodyIsADecodeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":`))
	}))
	defer server.Close()
	client, _ := newClient(t, server.URL, nil)
	var out map[string]any
	err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, &out)
	if KindOf(err) != KindDecode {
		t.Fatalf("kind = %q, want %q (err: %v)", KindOf(err), KindDecode, err)
	}
}

// --- Circuit breaker through the client ------------------------------------

func TestCircuitOpensAndShedsLoad(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, _ := newClient(t, server.URL, func(o *Options) {
		o.Retry.MaxAttempts = 1
		o.Breaker = BreakerSettings{FailureThreshold: 2, OpenFor: time.Hour, HalfOpenProbes: 1}
	})

	for i := 0; i < 2; i++ {
		if err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, nil); err == nil {
			t.Fatal("expected failure")
		}
	}
	if state := client.BreakerState(); state != BreakerOpen {
		t.Fatalf("state = %q, want %q", state, BreakerOpen)
	}

	before := atomic.LoadInt32(&attempts)
	err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, nil)
	if KindOf(err) != KindCircuitOpen {
		t.Fatalf("kind = %q, want %q", KindOf(err), KindCircuitOpen)
	}
	if atomic.LoadInt32(&attempts) != before {
		t.Fatal("an open circuit must not reach the upstream")
	}
}

// A rejected credential is deterministic. Retrying it will not help, and it
// says nothing about whether the upstream is up, so it must not trip the
// circuit and take out calls that would have succeeded.
func TestDeterministicFailuresDoNotOpenTheCircuit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client, _ := newClient(t, server.URL, func(o *Options) {
		o.Retry.MaxAttempts = 1
		o.Breaker = BreakerSettings{FailureThreshold: 2, OpenFor: time.Hour, HalfOpenProbes: 1}
	})
	for i := 0; i < 5; i++ {
		_ = client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, nil)
	}
	if state := client.BreakerState(); state != BreakerClosed {
		t.Fatalf("state = %q, want %q", state, BreakerClosed)
	}
}
