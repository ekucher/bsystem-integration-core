// Package operations defines the platform's infrastructure vocabulary: what a
// server is, what may be reported about one, and what a report changes.
//
// The platform does not monitor anything itself. Reports arrive from an
// operations toolkit — BRAVO in this deployment — and the platform normalizes,
// stores, publishes and notifies. That direction is deliberate: a platform
// that polled infrastructure would be a second monitoring system with its own
// idea of what "up" means, disagreeing with the one on call.
package operations

import (
	"errors"
	"strings"
	"time"
)

// Environments a server may be reported in. "unknown" is the honest default:
// a reporter that does not say must not be recorded as production, and must
// not be recorded as a lab either.
const (
	EnvUnknown = "unknown"
	EnvDev     = "dev"
	EnvStage   = "stage"
	EnvProd    = "prod"
)

// Statuses a server may hold.
const (
	StatusUnknown     = "unknown"
	StatusOK          = "ok"
	StatusWarning     = "warning"
	StatusError       = "error"
	StatusMaintenance = "maintenance"
	StatusOffline     = "offline"
)

// Event is one thing a reporter can say about a server.
type Event struct {
	// Severity is the platform's classification. A reporter may escalate it
	// but not lower it, the same rule notifications use.
	Severity string
	// Status is the server status this event implies, or empty when the
	// event says nothing about whether the server is up.
	Status string
}

// events is the vocabulary. An event outside it is refused rather than
// stored: an operations feed that silently accepts anything becomes a log,
// and nobody can write a query against a log whose contents are unbounded.
var events = map[string]Event{
	// A backup outcome says nothing about whether the server is up. This is
	// the distinction worth being careful about: a failed backup is a serious
	// event about a service the server runs, and recording it as "the server
	// is in error" would put a healthy machine on a dashboard as broken and
	// send someone to look at the wrong thing.
	"backup.succeeded": {Severity: "info", Status: ""},
	"backup.failed":    {Severity: "critical", Status: ""},

	// Likewise a self-test: it reports on what the server does, not on
	// whether it is reachable.
	"selftest.succeeded": {Severity: "info", Status: ""},
	"selftest.failed":    {Severity: "error", Status: ""},

	// Maintenance is a state the operator puts the server into, so it does
	// move the status — and completing it returns the server to ok rather
	// than to whatever it was before, because the operator has just finished
	// working on it and is asserting it is fine.
	"maintenance.started":   {Severity: "info", Status: StatusMaintenance},
	"maintenance.completed": {Severity: "info", Status: StatusOK},

	// These are statements about the server itself.
	"server.ok":      {Severity: "info", Status: StatusOK},
	"server.warning": {Severity: "warning", Status: StatusWarning},
	"server.error":   {Severity: "error", Status: StatusError},
	"server.offline": {Severity: "critical", Status: StatusOffline},
}

// Events returns the vocabulary, for documentation and contract tests.
func Events() map[string]Event {
	copied := make(map[string]Event, len(events))
	for name, event := range events {
		copied[name] = event
	}
	return copied
}

// ErrUnknownEvent means a reporter used a name outside the vocabulary.
var ErrUnknownEvent = errors.New("unknown operations event")

// Lookup returns the platform's classification of an event.
func Lookup(name string) (Event, error) {
	event, known := events[strings.TrimSpace(name)]
	if !known {
		return Event{}, ErrUnknownEvent
	}
	return event, nil
}

// NormalizeEnvironment maps a reported environment onto the platform's, and
// refuses one it does not recognise rather than guessing.
func NormalizeEnvironment(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return EnvUnknown, nil
	case EnvUnknown:
		return EnvUnknown, nil
	case EnvDev, "development":
		return EnvDev, nil
	case EnvStage, "staging":
		return EnvStage, nil
	case EnvProd, "production":
		return EnvProd, nil
	default:
		return "", errors.New("unknown environment: use dev, stage, prod or unknown")
	}
}

// Server is a normalized infrastructure host.
//
// It carries no hostname, address or any other way to reach the machine. The
// platform does not need one — it never connects to a server — and an
// inventory of reachable addresses is exactly the kind of internal topology
// that should not accumulate somewhere more people can read than can log in.
// Name is a label for humans; identity is the Global ID.
type Server struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	Status      string `json:"status"`
	// ClientID and ProjectID are the optional relations. An unresolvable one
	// is refused at report time rather than stored, because an operations
	// record attached to the wrong customer is worse than one attached to
	// none.
	ClientID  string `json:"client_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Source    string `json:"source"`
	SourceID  string `json:"source_id"`
	// LastEventAt is when the server was last reported on at all, which is
	// what tells an operator that a reporter has gone quiet.
	LastEventAt time.Time `json:"last_event_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// Report is one event about a server as it is stored and served.
type Report struct {
	ID            int64     `json:"id"`
	ServerID      string    `json:"server_id"`
	Event         string    `json:"event"`
	Severity      string    `json:"severity"`
	Summary       string    `json:"summary,omitempty"`
	Source        string    `json:"source"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// severityOrder mirrors the platform event severities.
var severityOrder = map[string]int{"debug": 0, "info": 1, "warning": 2, "error": 3, "critical": 4}

// EscalateTo returns the more severe of the platform's classification and the
// reporter's, so a reporter can raise an event's severity but not lower one
// the platform treats as critical. An unrecognised severity is ignored rather
// than trusted.
func EscalateTo(floor, reported string) string {
	reported = strings.ToLower(strings.TrimSpace(reported))
	base, known := severityOrder[floor]
	if !known {
		return reported
	}
	if rank, ok := severityOrder[reported]; ok && rank > base {
		return reported
	}
	return floor
}
