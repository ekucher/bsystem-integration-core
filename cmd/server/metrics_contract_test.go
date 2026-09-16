package main

import (
	"bufio"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

// The metrics endpoint is a published interface with a consumer that cannot
// complain: the Grafana dashboard in bsystem-deploy
// (observability/grafana/bsystem-platform.json) and any alert built beside it.
//
// A query naming a series that does not exist, or grouping by a label the
// series does not carry, returns nothing. Prometheus does not warn, Grafana
// draws an empty panel, and during an incident an empty panel reads as "no
// traffic" rather than "wrong query" — which is the worst possible moment to
// discover a renamed label.
//
// So the exposition is pinned here: every series the platform publishes, its
// type, and the labels it carries. Renaming or dropping either fails this test
// instead of silently emptying a panel.

// series is one published metric family and the labels its samples carry.
type series struct {
	kind   string
	labels []string
}

// published is the contract. Each entry names a dashboard panel or alert that
// depends on it, so a future change can see what breaks before making it.
//
// Sampled series (those read from a pool, a registry or a connection at scrape
// time) always emit. Recorded series only emit once something has happened, so
// this test makes one observation on each before reading the exposition — which
// also exercises the recording path rather than only the declaration.
var published = map[string]series{
	// Dashboard: "Request rate by route", "Error rate by status class".
	"bsystem_http_requests_total": {"counter", []string{"method", "route", "status", "status_class"}},
	// Dashboard: "Request latency (p50/p95/p99)".
	"bsystem_http_request_duration_seconds": {"histogram", []string{"method", "route"}},
	// Dashboard: "Requests in flight".
	"bsystem_http_requests_in_flight": {"gauge", []string{"route"}},

	// Dashboard: "Upstream attempts by outcome", "Retry share".
	"bsystem_adapter_attempts_total": {"counter", []string{"adapter", "outcome", "retry"}},
	// Dashboard: "Upstream latency p95".
	"bsystem_adapter_attempt_duration_seconds": {"histogram", []string{"adapter"}},
	"bsystem_adapter_circuit_rejected_total":   {"counter", []string{"adapter"}},
	// Dashboard: "Circuit state".
	"bsystem_adapter_circuit_state": {"gauge", []string{"adapter", "state"}},
	"bsystem_adapter_ready":         {"gauge", []string{"adapter"}},

	// Dashboard: "Connection pool", "Pool acquisitions that waited",
	// "Dependency availability".
	"bsystem_database_pool_connections":    {"gauge", []string{"state"}},
	"bsystem_database_pool_acquires_total": {"counter", []string{"outcome"}},
	"bsystem_database_up":                  {"gauge", nil},
	"bsystem_nats_up":                      {"gauge", nil},

	// Dashboard: "Events published by outcome".
	"bsystem_events_published_total": {"counter", []string{"event", "outcome"}},

	// No panel yet, but each exists because its absence is invisible: a
	// pipeline that has silently stopped looks exactly like one with nothing
	// to do.
	"bsystem_notifications_raised_total": {"counter", []string{"event", "outcome"}},
	"bsystem_search_operations_total":    {"counter", []string{"operation", "outcome"}},
	"bsystem_operations_events_total":    {"counter", []string{"event", "severity"}},
	"bsystem_ai_requests_total":          {"counter", []string{"provider", "result"}},
}

// exposition is one scrape, parsed.
type exposition struct {
	kinds  map[string]string              // metric family -> declared type
	helps  map[string]int                 // metric family -> HELP lines seen
	types  map[string]int                 // metric family -> TYPE lines seen
	labels map[string]map[string]struct{} // sample name -> label names seen
	names  []string                       // sample names, in order
}

func parseExposition(t *testing.T, body string) exposition {
	t.Helper()

	parsed := exposition{
		kinds:  map[string]string{},
		helps:  map[string]int{},
		types:  map[string]int{},
		labels: map[string]map[string]struct{}{},
	}

	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			fields := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)
			if len(fields) != 2 || fields[1] == "" {
				t.Errorf("HELP line carries no description: %q", line)
				continue
			}
			parsed.helps[fields[0]]++
		case strings.HasPrefix(line, "# TYPE "):
			fields := strings.SplitN(strings.TrimPrefix(line, "# TYPE "), " ", 2)
			if len(fields) != 2 {
				t.Errorf("malformed TYPE line: %q", line)
				continue
			}
			parsed.types[fields[0]]++
			parsed.kinds[fields[0]] = fields[1]
		case strings.HasPrefix(line, "#"):
			t.Errorf("unrecognised comment line: %q", line)
		default:
			name, labels, ok := parseSample(line)
			if !ok {
				t.Errorf("malformed sample line: %q", line)
				continue
			}
			parsed.names = append(parsed.names, name)
			if parsed.labels[name] == nil {
				parsed.labels[name] = map[string]struct{}{}
			}
			for _, label := range labels {
				parsed.labels[name][label] = struct{}{}
			}
		}
	}
	return parsed
}

// parseSample splits `name{a="1",b="2"} 3` into its name and label names.
func parseSample(line string) (string, []string, bool) {
	value := strings.LastIndex(line, " ")
	if value < 0 {
		return "", nil, false
	}
	head := line[:value]
	if strings.TrimSpace(line[value+1:]) == "" {
		return "", nil, false
	}

	open := strings.Index(head, "{")
	if open < 0 {
		return head, nil, head != ""
	}
	if !strings.HasSuffix(head, "}") {
		return "", nil, false
	}
	name := head[:open]
	var labels []string
	for _, pair := range strings.Split(head[open+1:len(head)-1], ",") {
		key, rest, found := strings.Cut(pair, "=")
		if !found || !strings.HasPrefix(rest, `"`) || !strings.HasSuffix(rest, `"`) {
			return "", nil, false
		}
		labels = append(labels, key)
	}
	return name, labels, name != ""
}

// metricsApp builds the server with every series registered and one
// observation recorded on each, so the exposition shows what a running
// platform publishes rather than only what it declares.
//
// It is built once for the whole file: registerPlatformMetrics panics on a
// duplicate name, deliberately, because two HELP lines for one family make a
// scraper reject it. Subtests share the scrape rather than each taking their
// own server.
// breakeredProbe is an adapter that reports a circuit state. The disabled
// placeholders standing in for unconfigured integrations have no breaker, so
// without one registered bsystem_adapter_circuit_state publishes nothing —
// which is correct for such a deployment, and means the series can only be
// checked where a breaker actually exists.
type breakeredProbe struct {
	adapters.Mock
	state string
}

func (b breakeredProbe) BreakerState() string { return b.state }

func metricsApp(t *testing.T) (exposition, string) {
	t.Helper()

	application, handler := integrationApp(t, matrixPrincipals())
	application.registerPlatformMetrics()

	if err := adapterRegistry.Register(breakeredProbe{
		Mock: adapters.Mock{
			AdapterInfo:   adapters.Info{ID: "probe-breakered", Name: "Probe", Version: "0", Status: adapters.StatusReady},
			AdapterHealth: adapters.Health{Status: adapters.StatusReady},
		},
		state: "closed",
	}); err != nil {
		t.Fatalf("register breakered probe: %v", err)
	}

	// One observation per recorded series, through the real recording path —
	// so this pins what the platform emits, not only what it declares.
	upstream.Attempt("espocrm", "", false, 0.01)
	upstream.CircuitRejected("espocrm")
	eventsPublished.Inc("client.updated", "ok")
	notificationsRaised.Inc("client.updated", "ok")
	searchIndexed.Inc("index", "ok")
	operationsReported.Inc("server.offline", "high")
	aiRequests.Inc("stub", "ok")

	// A served request, so the HTTP series describe completed work. They are
	// recorded when the handler returns, so a scrape that is itself the first
	// request finds the counter empty — which is exactly what an operator sees
	// on a freshly started platform, and why a panel showing "No data" there
	// is not evidence of a broken query.
	call(t, handler, http.MethodGet, "/health", "")

	recorded := call(t, handler, http.MethodGet, "/metrics", "")
	if recorded.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d", recorded.Code)
	}
	body := recorded.Body.String()
	return parseExposition(t, body), body
}

// One server, one scrape, shared by the checks below. The registry is a
// package-level singleton and registerPlatformMetrics panics on a duplicate
// name — deliberately, because two HELP lines for one family make a scraper
// reject it — so it can be called exactly once per process.
func TestTheMetricsEndpointKeepsItsContract(t *testing.T) {
	parsed, body := metricsApp(t)

	// Every series the dashboard names must exist, with the type and the
	// labels its queries assume.
	t.Run("publishes what its consumers query", func(t *testing.T) {
		for name, want := range published {
			got, declared := parsed.kinds[name]
			if !declared {
				t.Errorf("%s is not published; any panel or alert naming it draws nothing", name)
				continue
			}
			if got != want.kind {
				t.Errorf("%s is declared %q, want %q", name, got, want.kind)
			}

			// A histogram publishes its samples under suffixed names; the
			// family itself never appears as a sample.
			sampleName := name
			if want.kind == "histogram" {
				sampleName = name + "_bucket"
			}
			seen, ok := parsed.labels[sampleName]
			if !ok {
				t.Errorf("%s declares a type but publishes no series", name)
				continue
			}

			var carried []string
			for label := range seen {
				if want.kind == "histogram" && label == "le" {
					continue // the bucket boundary, not a dimension of the metric
				}
				carried = append(carried, label)
			}
			sort.Strings(carried)
			if strings.Join(carried, ",") != strings.Join(want.labels, ",") {
				t.Errorf("%s carries labels %v, want %v — a query grouping by a missing label returns nothing",
					sampleName, carried, want.labels)
			}
		}
	})

	// Nothing may be published that the contract above does not describe. A
	// series nobody wrote down is one nobody watches, and it still costs
	// storage in every deployment that scrapes it.
	t.Run("publishes nothing undocumented", func(t *testing.T) {
		// Registered separately and deliberately outside the dashboard
		// contract: build identification and the schema level are read when
		// something looks wrong rather than graphed.
		operational := map[string]bool{
			"bsystem_build_info":                true,
			"bsystem_schema_migrations_applied": true,
			"bsystem_schema_migrations_drifted": true,
		}
		for name := range parsed.kinds {
			if _, known := published[name]; known || operational[name] {
				continue
			}
			t.Errorf("%s is published but not described in the contract", name)
		}
	})

	// Prometheus reserves the `_total` suffix for counters. A series that
	// names itself a counter while announcing another type misleads promtool,
	// recording-rule linters and anything that normalises OpenMetrics — and it
	// is exactly the mistake this file was written after finding.
	t.Run("names and types agree", func(t *testing.T) {
		for name, kind := range parsed.kinds {
			switch {
			case strings.HasSuffix(name, "_total") && kind != "counter":
				t.Errorf("%s ends in _total but is declared %q", name, kind)
			case kind == "counter" && !strings.HasSuffix(name, "_total"):
				t.Errorf("%s is a counter but does not end in _total", name)
			case strings.HasSuffix(name, "_seconds") && kind != "histogram":
				t.Errorf("%s ends in _seconds but is declared %q", name, kind)
			}
			if !strings.HasPrefix(name, "bsystem_") {
				t.Errorf("%s is published without the platform's prefix", name)
			}
		}
	})

	// A scraper rejects a family whose HELP or TYPE appears twice, and drops
	// samples it cannot attribute to a declared family. Both are
	// whole-endpoint failures rather than one bad series.
	t.Run("is well formed", func(t *testing.T) {
		for name, count := range parsed.helps {
			if count != 1 {
				t.Errorf("%s has %d HELP lines; a scraper rejects the family", name, count)
			}
		}
		for name, count := range parsed.types {
			if count != 1 {
				t.Errorf("%s has %d TYPE lines; a scraper rejects the family", name, count)
			}
			if parsed.helps[name] != 1 {
				t.Errorf("%s is typed but has no HELP", name)
			}
		}
		for _, name := range parsed.names {
			family := name
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if trimmed, cut := strings.CutSuffix(family, suffix); cut {
					if _, histogram := parsed.kinds[trimmed]; histogram {
						family = trimmed
					}
					break
				}
			}
			if _, declared := parsed.kinds[family]; !declared {
				t.Errorf("%s is sampled with no TYPE declared; a scraper drops it", name)
			}
		}
	})

	// A histogram is usable by histogram_quantile only if its buckets are
	// cumulative and closed by +Inf. Without the closing bucket the p99 the
	// dashboard draws is computed from an incomplete distribution — a wrong
	// number rather than a missing one, which is worse.
	t.Run("histograms answer quantile queries", func(t *testing.T) {
		for name, kind := range parsed.kinds {
			if kind != "histogram" {
				continue
			}
			buckets, observed := parsed.labels[name+"_bucket"]
			if !observed {
				continue // nothing observed on this histogram in this run
			}
			if _, hasLe := buckets["le"]; !hasLe {
				t.Errorf("%s_bucket carries no le label", name)
			}
			for _, suffix := range []string{"_sum", "_count"} {
				if _, ok := parsed.labels[name+suffix]; !ok {
					t.Errorf("%s has buckets but no %s; quantiles and averages need both", name, suffix)
				}
			}
			if !strings.Contains(body, name+`_bucket{`) || !strings.Contains(body, `le="+Inf"`) {
				t.Errorf(`%s_bucket has no le="+Inf"; every quantile drawn from it is wrong, not missing`, name)
			}
		}
	})
}
