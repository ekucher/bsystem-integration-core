package observability

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds the platform's metrics and renders them in the Prometheus
// text exposition format.
//
// It is written here rather than taken from a client library because the
// platform needs four metric shapes and a stable exposition, and a dependency
// that pulls a large transitive tree into a service that runs with database
// and event-bus credentials is a supply-chain decision that should buy more
// than this.
type Registry struct {
	mu      sync.RWMutex
	metrics []metric
	byName  map[string]metric
}

type metric interface {
	name() string
	writeTo(w io.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]metric{}}
}

func (r *Registry) register(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[m.name()]; exists {
		// Registering the same metric twice would emit two HELP lines for one
		// name, which a scraper rejects.
		panic("observability: metric already registered: " + m.name())
	}
	r.byName[m.name()] = m
	r.metrics = append(r.metrics, m)
}

// Render writes every metric, in a stable order so that two scrapes of an
// unchanged process produce identical output and can be diffed.
//
// It is deliberately not named WriteTo: that name belongs to io.WriterTo,
// whose contract is to report bytes written and an error, and a metrics
// registry is not one.
func (r *Registry) Render(w io.Writer) {
	r.mu.RLock()
	metrics := make([]metric, len(r.metrics))
	copy(metrics, r.metrics)
	r.mu.RUnlock()

	sort.SliceStable(metrics, func(i, j int) bool { return metrics[i].name() < metrics[j].name() })
	for _, m := range metrics {
		m.writeTo(w)
	}
}

// --- Labels -----------------------------------------------------------------

// labelSet is a rendered label list, used as a map key so that a series is
// identified by its labels rather than by insertion order.
type labelSet string

func makeLabelSet(names, values []string) labelSet {
	if len(names) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(names))
	for i, name := range names {
		value := ""
		if i < len(values) {
			value = values[i]
		}
		// Quoted explicitly rather than with %q: Go's quoting escapes more
		// than the exposition format defines, and a tab or a non-ASCII
		// character would be rendered as a Go escape a scraper does not read.
		pairs = append(pairs, fmt.Sprintf(`%s="%s"`, name, escapeLabelValue(value)))
	}
	sort.Strings(pairs)
	return labelSet("{" + strings.Join(pairs, ",") + "}")
}

// escapeLabelValue escapes the characters the exposition format reserves. A
// label value carrying a raw newline or quote would produce output a scraper
// cannot parse.
func escapeLabelValue(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

// withExtra renders a label set plus one more label, used for a histogram's
// bucket boundary.
func (l labelSet) withExtra(name, value string) string {
	pair := fmt.Sprintf(`%s="%s"`, name, escapeLabelValue(value))
	if l == "" {
		return "{" + pair + "}"
	}
	return string(l[:len(l)-1]) + "," + pair + "}"
}

// --- Counter ----------------------------------------------------------------

// CounterVec is a set of monotonically increasing counters.
type CounterVec struct {
	metricName string
	help       string
	labels     []string

	mu     sync.Mutex
	values map[labelSet]float64
}

// Counter registers a counter.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	counter := &CounterVec{metricName: name, help: help, labels: labels, values: map[labelSet]float64{}}
	r.register(counter)
	return counter
}

func (c *CounterVec) name() string { return c.metricName }

// Inc adds one to the series identified by values.
func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Add increases the series identified by values.
func (c *CounterVec) Add(delta float64, values ...string) {
	key := makeLabelSet(c.labels, values)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] += delta
}

// Value returns the current value of a series, for tests.
func (c *CounterVec) Value(values ...string) float64 {
	key := makeLabelSet(c.labels, values)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

func (c *CounterVec) writeTo(w io.Writer) {
	c.mu.Lock()
	snapshot := make(map[labelSet]float64, len(c.values))
	for key, value := range c.values {
		snapshot[key] = value
	}
	c.mu.Unlock()

	writeHeader(w, c.metricName, c.help, "counter")
	for _, key := range sortedLabelSets(snapshot) {
		fmt.Fprintf(w, "%s%s %s\n", c.metricName, key, formatFloat(snapshot[key]))
	}
}

// --- Gauge ------------------------------------------------------------------

// GaugeVec is a set of values that can go up and down.
type GaugeVec struct {
	metricName string
	help       string
	labels     []string

	mu     sync.Mutex
	values map[labelSet]float64
}

// Gauge registers a gauge.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	gauge := &GaugeVec{metricName: name, help: help, labels: labels, values: map[labelSet]float64{}}
	r.register(gauge)
	return gauge
}

func (g *GaugeVec) name() string { return g.metricName }

// Set replaces the value of a series.
func (g *GaugeVec) Set(value float64, labelValues ...string) {
	key := makeLabelSet(g.labels, labelValues)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values[key] = value
}

// Add adjusts a series relative to its current value.
//
// A concurrent count — requests in flight, say — must be adjusted rather than
// set: with Set, the first of several overlapping requests to finish would
// zero the gauge while the others are still running.
func (g *GaugeVec) Add(delta float64, labelValues ...string) {
	key := makeLabelSet(g.labels, labelValues)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values[key] += delta
}

// Value returns the current value of a series, for tests.
func (g *GaugeVec) Value(labelValues ...string) float64 {
	key := makeLabelSet(g.labels, labelValues)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.values[key]
}

func (g *GaugeVec) writeTo(w io.Writer) {
	g.mu.Lock()
	snapshot := make(map[labelSet]float64, len(g.values))
	for key, value := range g.values {
		snapshot[key] = value
	}
	g.mu.Unlock()

	writeHeader(w, g.metricName, g.help, "gauge")
	for _, key := range sortedLabelSets(snapshot) {
		fmt.Fprintf(w, "%s%s %s\n", g.metricName, key, formatFloat(snapshot[key]))
	}
}

// --- GaugeFunc --------------------------------------------------------------

// Sample is one observation from a sampled gauge: the label values
// identifying the series, and its value.
type Sample struct {
	Labels []string
	Value  float64
}

// GaugeFunc is a gauge sampled at scrape time.
//
// Pool sizes, connection states and circuit states are read from whatever
// owns them rather than mirrored into a variable, because a mirrored value
// drifts out of date exactly when it matters.
type GaugeFunc struct {
	metricName string
	help       string
	labels     []string
	sample     func() []Sample
}

// GaugeFunc registers a gauge sampled when the registry is rendered.
func (r *Registry) GaugeFunc(name, help string, labels []string, sample func() []Sample) *GaugeFunc {
	gauge := &GaugeFunc{metricName: name, help: help, labels: labels, sample: sample}
	r.register(gauge)
	return gauge
}

func (g *GaugeFunc) name() string { return g.metricName }

func (g *GaugeFunc) writeTo(w io.Writer) {
	writeHeader(w, g.metricName, g.help, "gauge")

	samples := g.sample()
	rendered := make(map[labelSet]float64, len(samples))
	for _, s := range samples {
		rendered[makeLabelSet(g.labels, s.Labels)] = s.Value
	}
	for _, key := range sortedLabelSets(rendered) {
		fmt.Fprintf(w, "%s%s %s\n", g.metricName, key, formatFloat(rendered[key]))
	}
}

// --- Histogram --------------------------------------------------------------

// DefaultDurationBuckets covers the range BSYSTEM request latencies fall in:
// fast local reads at the bottom, and the upstream timeout at the top so that
// a request being cut off by the adapter bound is visible as a bucket rather
// than only in the +Inf overflow.
var DefaultDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// HistogramVec is a set of latency distributions.
type HistogramVec struct {
	metricName string
	help       string
	labels     []string
	buckets    []float64

	mu     sync.Mutex
	series map[labelSet]*histogram
}

type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

// Histogram registers a histogram. Buckets must be sorted ascending; the
// implicit +Inf bucket is added automatically.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	sorted := append([]float64(nil), buckets...)
	sort.Float64s(sorted)
	h := &HistogramVec{metricName: name, help: help, labels: labels, buckets: sorted, series: map[labelSet]*histogram{}}
	r.register(h)
	return h
}

func (h *HistogramVec) name() string { return h.metricName }

// Observe records one measurement.
func (h *HistogramVec) Observe(value float64, labelValues ...string) {
	key := makeLabelSet(h.labels, labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()

	series, exists := h.series[key]
	if !exists {
		series = &histogram{counts: make([]uint64, len(h.buckets))}
		h.series[key] = series
	}
	series.sum += value
	series.count++
	for i, bound := range h.buckets {
		if value <= bound {
			series.counts[i]++
		}
	}
}

func (h *HistogramVec) writeTo(w io.Writer) {
	h.mu.Lock()
	keys := make([]labelSet, 0, len(h.series))
	for key := range h.series {
		keys = append(keys, key)
	}
	snapshot := make(map[labelSet]histogram, len(h.series))
	for key, series := range h.series {
		counts := append([]uint64(nil), series.counts...)
		snapshot[key] = histogram{counts: counts, sum: series.sum, count: series.count}
	}
	h.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	writeHeader(w, h.metricName, h.help, "histogram")
	for _, key := range keys {
		series := snapshot[key]
		// Prometheus histogram buckets are cumulative: each _bucket is the
		// count of observations less than or equal to its boundary.
		for i, bound := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, key.withExtra("le", formatFloat(bound)), series.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, key.withExtra("le", "+Inf"), series.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.metricName, key, formatFloat(series.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.metricName, key, series.count)
	}
}

// --- Shared -----------------------------------------------------------------

func writeHeader(w io.Writer, name, help, kind string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, strings.ReplaceAll(help, "\n", " "))
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
}

func sortedLabelSets(values map[labelSet]float64) []labelSet {
	keys := make([]labelSet, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// formatFloat renders a value the way the exposition format expects: integers
// without a decimal point, and no exponent for ordinary magnitudes.
func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
