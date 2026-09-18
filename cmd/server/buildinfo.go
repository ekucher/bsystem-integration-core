package main

import (
	gocontext "context"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/observability"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// Build identity, set at link time:
//
//	go build -ldflags "-X main.buildVersion=v1.2.3 -X main.buildCommit=$(git rev-parse HEAD) -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// Nothing here is a secret: a commit SHA identifies public source. Anything
// that would be sensitive — a hostname, a credential, a customer — is
// deliberately absent, because build metadata is the one thing an operator
// pastes into a ticket without thinking about it.
var (
	buildVersion = "0.6.0"
	buildCommit  = ""
	buildDate    = ""
)

// buildInfo is the release identity of this binary.
type buildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	// SchemaEmbedded is the highest migration this binary carries.
	SchemaEmbedded string `json:"schema_embedded"`
}

// currentBuild resolves build identity, falling back to what the Go toolchain
// stamps into the binary. A build made without -ldflags still reports its
// commit when it was built from a clean checkout, which is most of the time.
func currentBuild() buildInfo {
	info := buildInfo{Version: buildVersion, Commit: buildCommit, Date: buildDate}
	if info.Commit == "" || info.Date == "" {
		if stamped, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range stamped.Settings {
				switch setting.Key {
				case "vcs.revision":
					if info.Commit == "" {
						info.Commit = setting.Value
					}
				case "vcs.time":
					if info.Date == "" {
						info.Date = setting.Value
					}
				}
			}
		}
	}
	if embedded, err := platformdb.EmbeddedSchemaLevel(); err == nil {
		info.SchemaEmbedded = embedded.Level
	}
	return info
}

// shortCommit keeps a metric label readable without losing identity.
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// registerBuildMetrics exposes build identity the way Prometheus expects it:
// a gauge fixed at 1 whose labels carry the information.
//
// This is the safe place for it. /metrics is an operational endpoint, the
// labels name public source, and an operator can answer "which commit is this
// environment running, and is its database at the matching level?" without
// anyone handing out an API token.
func (a *app) registerBuildMetrics() {
	info := currentBuild()

	metricsRegistry.GaugeFunc(
		"bsystem_build_info",
		"Build identity of the running Integration Core (always 1).",
		[]string{"version", "commit", "built_at", "schema_embedded"},
		func() []observability.Sample {
			return []observability.Sample{{
				Labels: []string{info.Version, shortCommit(info.Commit), info.Date, info.SchemaEmbedded},
				Value:  1,
			}}
		},
	)

	// The applied level is read from the database rather than from the
	// binary, so the two series disagree exactly when the database is behind
	// the code — the state that explains an otherwise inexplicable failure
	// after a deployment.
	metricsRegistry.GaugeFunc(
		"bsystem_schema_migrations_applied",
		"Number of migrations recorded as applied in the connected database.",
		[]string{"level"},
		func() []observability.Sample {
			ctx, cancel := gocontext.WithTimeout(gocontext.Background(), 2*time.Second)
			defer cancel()
			state, err := a.db.SchemaLevel(ctx)
			if err != nil {
				return nil
			}
			return []observability.Sample{{Labels: []string{state.Level}, Value: float64(state.Applied)}}
		},
	)

	// Drift is invisible to every other series here. An edited migration keeps
	// its filename, so the level and the applied count are identical to a
	// database that never diverged — this is the only number that differs.
	// Zero is published rather than nothing, so an operator can tell "no
	// drift" from "nobody is reporting".
	metricsRegistry.GaugeFunc(
		"bsystem_schema_migrations_drifted",
		"Migrations whose file no longer hashes to what this database applied.",
		nil,
		func() []observability.Sample {
			ctx, cancel := gocontext.WithTimeout(gocontext.Background(), 2*time.Second)
			defer cancel()
			state, err := a.db.SchemaLevel(ctx)
			if err != nil {
				return nil
			}
			return []observability.Sample{{Value: float64(len(state.Drifted))}}
		},
	)
}
