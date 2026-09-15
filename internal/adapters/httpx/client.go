package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

// RetryPolicy bounds how hard an adapter tries.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first. One
	// disables retrying.
	MaxAttempts int
	// BaseDelay is the first backoff interval; each attempt doubles it.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff interval, including one derived from a
	// Retry-After header.
	MaxDelay time.Duration
	// sleep and jitter are injectable so tests can drive retries
	// deterministically instead of waiting.
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// DefaultRetryPolicy is the policy adapters use unless told otherwise.
//
// Three attempts with a short base delay keeps a transient blip invisible to
// the caller while staying well inside the adapter timeout, so retrying can
// never turn a slow upstream into a request that outlives its own deadline.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second}
}

// Options configures a Client.
type Options struct {
	// Adapter is the adapter id, used in errors and metrics. Required.
	Adapter string
	// BaseURL is the upstream root. Required.
	BaseURL string
	// Timeout bounds a single attempt.
	Timeout time.Duration
	// MaxBodyBytes bounds how much of a response is read, so a hostile or
	// broken upstream cannot exhaust the platform's memory.
	MaxBodyBytes int64
	Retry        RetryPolicy
	Breaker      BreakerSettings
	// Transport is injectable for tests.
	Transport http.RoundTripper
	// Recorder observes attempts. Optional.
	Recorder adapters.Recorder
}

// Client performs bounded, retrying, circuit-broken JSON calls to one
// upstream.
type Client struct {
	adapter      string
	baseURL      *url.URL
	http         *http.Client
	maxBodyBytes int64
	retry        RetryPolicy
	breaker      *Breaker
	recorder     adapters.Recorder
}

const (
	defaultTimeout      = 10 * time.Second
	defaultMaxBodyBytes = 8 << 20
)

// New validates the options and returns a client.
func New(options Options) (*Client, error) {
	adapter := strings.TrimSpace(options.Adapter)
	if adapter == "" {
		return nil, errors.New("httpx: adapter id is required")
	}
	raw := strings.TrimSpace(options.BaseURL)
	if raw == "" {
		return nil, errors.New("httpx: base URL is required")
	}
	parsed, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("httpx: invalid base URL")
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}
	if options.MaxBodyBytes <= 0 {
		options.MaxBodyBytes = defaultMaxBodyBytes
	}
	if options.Retry.MaxAttempts <= 0 {
		options.Retry = DefaultRetryPolicy()
	}
	if options.Retry.BaseDelay <= 0 {
		options.Retry.BaseDelay = DefaultRetryPolicy().BaseDelay
	}
	if options.Retry.MaxDelay <= 0 {
		options.Retry.MaxDelay = DefaultRetryPolicy().MaxDelay
	}
	if options.Retry.sleep == nil {
		options.Retry.sleep = sleepContext
	}
	if options.Retry.jitter == nil {
		options.Retry.jitter = fullJitter
	}
	if options.Breaker.FailureThreshold == 0 && options.Breaker.OpenFor == 0 {
		options.Breaker = DefaultBreakerSettings()
	}

	// The per-attempt timeout lives on the request context rather than on
	// http.Client, so that retries share one overall budget and a caller's
	// cancellation still wins immediately.
	return &Client{
		adapter:      adapter,
		baseURL:      parsed,
		http:         &http.Client{Transport: options.Transport, Timeout: options.Timeout},
		maxBodyBytes: options.MaxBodyBytes,
		retry:        options.Retry,
		breaker:      NewBreaker(options.Breaker),
		recorder:     options.Recorder,
	}, nil
}

// Request is one upstream call.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	// Body is marshalled as JSON when non-nil.
	Body any
	// Header carries upstream credentials. It is never logged or rendered
	// into an error.
	Header http.Header
	// Idempotent marks a request that may be retried.
	//
	// It is explicit rather than derived from the method because some
	// upstreams — Outline's RPC API among them — perform reads over POST.
	// Retrying a non-idempotent request risks duplicating an effect, so the
	// default is not to.
	Idempotent bool
}

// BreakerState reports the adapter's circuit state, for health and metrics.
func (c *Client) BreakerState() BreakerState { return c.breaker.State() }

// BreakerStats reports the adapter's circuit counters.
func (c *Client) BreakerStats() Stats { return c.breaker.Stats() }

// Do performs the request, decoding a JSON response into out when out is
// non-nil. It retries only idempotent requests, only for failures that could
// plausibly succeed on another attempt, and only within the policy's bounds.
func (c *Client) Do(ctx context.Context, request Request, out any) error {
	if !c.breaker.Allow() {
		if c.recorder != nil {
			c.recorder.CircuitRejected(c.adapter)
		}
		return &Error{Adapter: c.adapter, Kind: KindCircuitOpen}
	}

	attempts := 1
	if request.Idempotent {
		attempts = c.retry.MaxAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		started := time.Now()
		err := c.attempt(ctx, request, out)
		c.record(err, attempt > 1, time.Since(started))

		if err == nil {
			c.breaker.Succeed()
			return nil
		}
		lastErr = err

		var upstream *Error
		if !errors.As(err, &upstream) || !upstream.retryable() {
			// A deterministic failure says nothing about availability, so it
			// must not count towards opening the circuit.
			c.breaker.Fail(false)
			return err
		}
		if attempt == attempts {
			break
		}
		if waitErr := c.retry.sleep(ctx, c.backoff(attempt, upstream.RetryAfter)); waitErr != nil {
			c.breaker.Fail(true)
			return &Error{Adapter: c.adapter, Kind: KindCanceled, cause: waitErr}
		}
	}

	c.breaker.Fail(true)
	return lastErr
}

// record reports one attempt's outcome, if anything is listening.
func (c *Client) record(err error, retry bool, elapsed time.Duration) {
	if c.recorder == nil {
		return
	}
	kind := Kind("")
	var upstream *Error
	if errors.As(err, &upstream) {
		kind = upstream.Kind
	} else if err != nil {
		kind = KindUnavailable
	}
	c.recorder.Attempt(c.adapter, string(kind), retry, elapsed.Seconds())
}

// backoff is exponential with full jitter, capped by MaxDelay.
//
// Jitter matters more than the growth curve here: without it, every adapter
// instance that failed together retries together, and the upstream is hit by
// the same synchronised burst that knocked it over.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		// The upstream told us how long to wait. Honour it, but never longer
		// than the policy allows, so a hostile or mistaken header cannot
		// stall the platform.
		if retryAfter > c.retry.MaxDelay {
			return c.retry.MaxDelay
		}
		return retryAfter
	}
	delay := c.retry.BaseDelay << (attempt - 1)
	if delay > c.retry.MaxDelay || delay <= 0 {
		delay = c.retry.MaxDelay
	}
	return c.retry.jitter(delay)
}

func (c *Client) attempt(ctx context.Context, request Request, out any) error {
	var body io.Reader
	if request.Body != nil {
		encoded, err := json.Marshal(request.Body)
		if err != nil {
			return &Error{Adapter: c.adapter, Kind: KindDecode, cause: err}
		}
		body = bytes.NewReader(encoded)
	}

	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + request.Path
	if request.Query != nil {
		target.RawQuery = request.Query.Encode()
	}

	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, target.String(), body)
	if err != nil {
		return &Error{Adapter: c.adapter, Kind: KindUnavailable, cause: err}
	}
	httpRequest.Header.Set("Accept", "application/json")
	if request.Body != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	for name, values := range request.Header {
		for _, value := range values {
			httpRequest.Header.Add(name, value)
		}
	}

	response, err := c.http.Do(httpRequest)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The body is drained but never read into the error: an upstream
		// error page can contain internal hostnames and confidential content.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.maxBodyBytes))
		return &Error{
			Adapter:    c.adapter,
			Kind:       kindForStatus(response.StatusCode),
			Status:     response.StatusCode,
			RetryAfter: retryAfter(response.Header),
		}
	}
	if out == nil {
		return nil
	}

	// One byte past the bound is read so that hitting it is distinguishable
	// from a response that merely ends there.
	limited := io.LimitReader(response.Body, c.maxBodyBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return c.transportError(ctx, err)
	}
	if int64(len(payload)) > c.maxBodyBytes {
		return &Error{Adapter: c.adapter, Kind: KindTooLarge, Status: response.StatusCode}
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return &Error{Adapter: c.adapter, Kind: KindDecode, Status: response.StatusCode, cause: err}
	}
	return nil
}

// transportError distinguishes the caller giving up from the upstream failing
// to answer. Only the latter is the upstream's fault, and only the latter is
// worth retrying.
func (c *Client) transportError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
		return &Error{Adapter: c.adapter, Kind: KindCanceled, cause: err}
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return &Error{Adapter: c.adapter, Kind: KindTimeout, cause: err}
	case errors.Is(err, context.Canceled):
		return &Error{Adapter: c.adapter, Kind: KindCanceled, cause: err}
	default:
		return &Error{Adapter: c.adapter, Kind: KindUnavailable, cause: err}
	}
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// retryAfter reads the Retry-After header in either of its forms.
func retryAfter(header http.Header) time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

// fullJitter spreads retries across the whole interval rather than clustering
// them at its end.
func fullJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(delay)) + 1)
}

// sleepContext waits, but gives up as soon as the caller does.
func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// OptionsFor turns an adapter configuration into client options, applying the
// package defaults for anything the configuration leaves unset.
func OptionsFor(adapter string, config adapters.Config) Options {
	options := Options{
		Adapter:      adapter,
		BaseURL:      config.BaseURL,
		Timeout:      config.Timeout,
		MaxBodyBytes: config.MaxBodyBytes,
		Retry:        DefaultRetryPolicy(),
		Breaker:      DefaultBreakerSettings(),
		Recorder:     config.Recorder,
	}
	if config.RetryAttempts > 0 {
		options.Retry.MaxAttempts = config.RetryAttempts
	}
	switch {
	case config.CircuitFailureThreshold < 0:
		// Negative disables the breaker outright, which a deployment may want
		// when something upstream already sheds load.
		options.Breaker.FailureThreshold = 0
	case config.CircuitFailureThreshold > 0:
		options.Breaker.FailureThreshold = config.CircuitFailureThreshold
	}
	if config.CircuitOpenFor > 0 {
		options.Breaker.OpenFor = config.CircuitOpenFor
	}
	return options
}
