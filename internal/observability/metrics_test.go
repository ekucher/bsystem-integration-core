package observability

import (
	"strings"
	"sync"
	"testing"
)

func render(registry *Registry) string {
	var builder strings.Builder
	registry.Render(&builder)
	return builder.String()
}

func TestCounterExposition(t *testing.T) {
	registry := NewRegistry()
	requests := registry.Counter("bsystem_requests_total", "Requests handled.", "route", "status")

	requests.Inc("/api/v1/clients", "200")
	requests.Inc("/api/v1/clients", "200")
	requests.Inc("/api/v1/clients", "403")

	output := render(registry)
	for _, expected := range []string{
		"# HELP bsystem_requests_total Requests handled.",
		"# TYPE bsystem_requests_total counter",
		`bsystem_requests_total{route="/api/v1/clients",status="200"} 2`,
		`bsystem_requests_total{route="/api/v1/clients",status="403"} 1`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("missing %q in:\n%s", expected, output)
		}
	}
}

func TestCounterWithoutLabels(t *testing.T) {
	registry := NewRegistry()
	registry.Counter("bsystem_events_total", "Events.").Add(3)
	if !strings.Contains(render(registry), "bsystem_events_total 3\n") {
		t.Fatalf("unlabelled counter rendered wrongly:\n%s", render(registry))
	}
}

func TestGauge(t *testing.T) {
	registry := NewRegistry()
	gauge := registry.Gauge("bsystem_pool_conns", "Connections.", "state")
	gauge.Set(4, "idle")
	gauge.Set(9, "acquired")
	gauge.Set(5, "idle") // Replaces rather than accumulates.

	output := render(registry)
	if !strings.Contains(output, `bsystem_pool_conns{state="idle"} 5`) {
		t.Fatalf("gauge did not replace its value:\n%s", output)
	}
	if !strings.Contains(output, `bsystem_pool_conns{state="acquired"} 9`) {
		t.Fatalf("missing series:\n%s", output)
	}
}

// A sampled gauge reads from whatever owns the value, so it cannot drift out
// of date the way a mirrored variable can.
func TestGaugeFuncIsSampledAtRender(t *testing.T) {
	registry := NewRegistry()
	value := 1.0
	registry.GaugeFunc("bsystem_live", "Live value.", nil, func() []Sample {
		return []Sample{{Value: value}}
	})

	if !strings.Contains(render(registry), "bsystem_live 1\n") {
		t.Fatalf("first render:\n%s", render(registry))
	}
	value = 7
	if !strings.Contains(render(registry), "bsystem_live 7\n") {
		t.Fatalf("second render did not resample:\n%s", render(registry))
	}
}

// A sampled gauge often needs more than one label: a circuit state is
// identified by both the adapter and the state, and squeezing them into one
// label would make the series ungroupable.
func TestGaugeFuncWithSeveralLabels(t *testing.T) {
	registry := NewRegistry()
	registry.GaugeFunc("bsystem_circuit", "Circuit state.", []string{"adapter", "state"}, func() []Sample {
		return []Sample{
			{Labels: []string{"espocrm", "closed"}, Value: 1},
			{Labels: []string{"espocrm", "open"}, Value: 0},
		}
	})
	output := render(registry)
	if !strings.Contains(output, `bsystem_circuit{adapter="espocrm",state="closed"} 1`) {
		t.Fatalf("multi-label sampled gauge:\n%s", output)
	}
	if !strings.Contains(output, `bsystem_circuit{adapter="espocrm",state="open"} 0`) {
		t.Fatalf("multi-label sampled gauge:\n%s", output)
	}
}

func TestGaugeFuncWithLabels(t *testing.T) {
	registry := NewRegistry()
	registry.GaugeFunc("bsystem_adapter_up", "Adapter availability.", []string{"adapter"}, func() []Sample {
		return []Sample{{Labels: []string{"espocrm"}, Value: 1}, {Labels: []string{"redmine"}, Value: 0}}
	})
	output := render(registry)
	if !strings.Contains(output, `bsystem_adapter_up{adapter="espocrm"} 1`) || !strings.Contains(output, `bsystem_adapter_up{adapter="redmine"} 0`) {
		t.Fatalf("labelled sampled gauge:\n%s", output)
	}
}

// Prometheus histogram buckets are cumulative, and the exposition must carry
// every boundary plus +Inf, _sum and _count. Getting this wrong produces
// output a scraper accepts and then charts incorrectly, which is worse than
// output it rejects.
func TestHistogramExposition(t *testing.T) {
	registry := NewRegistry()
	latency := registry.Histogram("bsystem_latency_seconds", "Latency.", []float64{0.1, 0.5, 1}, "route")

	for _, value := range []float64{0.05, 0.2, 0.2, 0.75, 4} {
		latency.Observe(value, "/api/v1/me")
	}

	output := render(registry)
	expected := []string{
		"# TYPE bsystem_latency_seconds histogram",
		`bsystem_latency_seconds_bucket{route="/api/v1/me",le="0.1"} 1`,
		`bsystem_latency_seconds_bucket{route="/api/v1/me",le="0.5"} 3`,
		`bsystem_latency_seconds_bucket{route="/api/v1/me",le="1"} 4`,
		`bsystem_latency_seconds_bucket{route="/api/v1/me",le="+Inf"} 5`,
		`bsystem_latency_seconds_count{route="/api/v1/me"} 5`,
	}
	for _, line := range expected {
		if !strings.Contains(output, line) {
			t.Fatalf("missing %q in:\n%s", line, output)
		}
	}
	if !strings.Contains(output, `bsystem_latency_seconds_sum{route="/api/v1/me"} 5.2`) {
		t.Fatalf("sum is wrong:\n%s", output)
	}
}

// Every bucket count must be at least the one below it. A non-monotonic
// histogram is silently meaningless.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	registry := NewRegistry()
	latency := registry.Histogram("bsystem_latency_seconds", "Latency.", DefaultDurationBuckets)
	for _, value := range []float64{0.001, 0.02, 0.3, 0.3, 2, 30} {
		latency.Observe(value)
	}

	var previous uint64
	for _, line := range strings.Split(render(registry), "\n") {
		if !strings.HasPrefix(line, "bsystem_latency_seconds_bucket") {
			continue
		}
		fields := strings.Fields(line)
		var count uint64
		if _, err := fmtSscan(fields[len(fields)-1], &count); err != nil {
			t.Fatalf("unparseable bucket count in %q", line)
		}
		if count < previous {
			t.Fatalf("bucket counts are not cumulative: %d after %d in %q", count, previous, line)
		}
		previous = count
	}
	if previous != 6 {
		t.Fatalf("final bucket = %d, want 6 observations", previous)
	}
}

func TestHistogramSortsUnsortedBuckets(t *testing.T) {
	registry := NewRegistry()
	latency := registry.Histogram("bsystem_latency_seconds", "Latency.", []float64{1, 0.1, 0.5})
	latency.Observe(0.2)

	output := render(registry)
	first := strings.Index(output, `le="0.1"`)
	second := strings.Index(output, `le="0.5"`)
	third := strings.Index(output, `le="1"`)
	if !(first < second && second < third) {
		t.Fatalf("buckets are not ascending:\n%s", output)
	}
}

// A label value carrying a quote or a newline would produce output no scraper
// can parse.
func TestLabelValuesAreEscaped(t *testing.T) {
	registry := NewRegistry()
	counter := registry.Counter("bsystem_odd_total", "Odd.", "value")
	counter.Inc(`say "hi"`)
	counter.Inc("line\nbreak")
	counter.Inc(`back\slash`)

	output := render(registry)
	for _, expected := range []string{`value="say \"hi\""`, `value="line\nbreak"`, `value="back\\slash"`} {
		if !strings.Contains(output, expected) {
			t.Fatalf("missing %q in:\n%s", expected, output)
		}
	}
	// A raw newline would split a sample across two lines.
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "bsystem_odd_total") && !strings.HasSuffix(strings.TrimSpace(line), "1") {
			t.Fatalf("sample line is malformed: %q", line)
		}
	}
}

// Two HELP lines for one metric name make a scrape invalid, so a duplicate
// registration must fail loudly at startup rather than quietly at scrape time.
func TestDuplicateRegistrationPanics(t *testing.T) {
	registry := NewRegistry()
	registry.Counter("bsystem_total", "First.")
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate metric name must panic")
		}
	}()
	registry.Counter("bsystem_total", "Second.")
}

// Rendering is stable so that two scrapes of an unchanged process are
// identical and can be diffed during an incident.
func TestRenderIsStable(t *testing.T) {
	registry := NewRegistry()
	counter := registry.Counter("bsystem_b_total", "B.", "one", "two")
	registry.Counter("bsystem_a_total", "A.").Inc()
	counter.Inc("x", "y")
	counter.Inc("p", "q")

	first := render(registry)
	if first != render(registry) {
		t.Fatal("two renders of an unchanged registry differ")
	}
	if strings.Index(first, "bsystem_a_total") > strings.Index(first, "bsystem_b_total") {
		t.Fatalf("metrics are not ordered by name:\n%s", first)
	}
}

// Metrics are written from every request-handling goroutine at once.
func TestMetricsAreSafeUnderConcurrency(t *testing.T) {
	registry := NewRegistry()
	counter := registry.Counter("bsystem_requests_total", "Requests.", "route")
	latency := registry.Histogram("bsystem_latency_seconds", "Latency.", DefaultDurationBuckets, "route")
	gauge := registry.Gauge("bsystem_inflight", "In flight.", "route")

	var waiting sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		waiting.Add(1)
		go func(worker int) {
			defer waiting.Done()
			for i := 0; i < 250; i++ {
				route := "/api/v1/clients"
				counter.Inc(route)
				latency.Observe(float64(i%10)/100, route)
				gauge.Set(float64(i), route)
				_ = render(registry)
			}
		}(worker)
	}
	waiting.Wait()

	if got := counter.Value("/api/v1/clients"); got != 2000 {
		t.Fatalf("counter = %v, want 2000", got)
	}
}

// fmtSscan keeps the bucket test readable without importing fmt for one call.
func fmtSscan(text string, target *uint64) (int, error) {
	var value uint64
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, errNotANumber
		}
		value = value*10 + uint64(r-'0')
	}
	*target = value
	return 1, nil
}

var errNotANumber = &parseError{}

type parseError struct{}

func (e *parseError) Error() string { return "not a number" }

// A concurrent count must be adjusted rather than replaced: with Set, the
// first of several overlapping operations to finish would zero the gauge
// while the others are still running.
func TestGaugeAddTracksOverlappingWork(t *testing.T) {
	registry := NewRegistry()
	inFlight := registry.Gauge("bsystem_in_flight", "In flight.", "route")

	inFlight.Add(1, "/api/v1/clients")
	inFlight.Add(1, "/api/v1/clients")
	inFlight.Add(-1, "/api/v1/clients")

	if got := inFlight.Value("/api/v1/clients"); got != 1 {
		t.Fatalf("in flight = %v, want 1: one of two overlapping requests is still running", got)
	}

	inFlight.Add(-1, "/api/v1/clients")
	if got := inFlight.Value("/api/v1/clients"); got != 0 {
		t.Fatalf("in flight = %v, want 0", got)
	}
}
