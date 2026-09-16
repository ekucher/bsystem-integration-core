// Package support defines the platform's support vocabulary: incidents and
// requests, how they move between statuses, and how an SLA state is derived.
//
// The part worth reading carefully is the SLA. This package computes an SLA
// state; it does not decide what the targets are. Those are a commercial
// commitment — what the business promises a customer and what it owes when it
// misses — and inventing plausible-looking defaults would put numbers nobody
// agreed to in front of customers, where they read as a promise. With no
// policy configured, an incident has no due dates and its SLA state says so.
package support

import (
	"errors"
	"strings"
	"time"
)

// Kinds. An incident is something broken; a request is something wanted. They
// share a table because they share a lifecycle, a severity and an audience,
// and splitting them would duplicate all three to express one word.
const (
	KindIncident = "incident"
	KindRequest  = "request"
)

// Severities.
//
// These are deliberately not the platform's event severities. An event
// severity describes what happened; an incident severity is a statement about
// how quickly somebody will respond, which is why it is a smaller, blunter
// scale and why it is what an SLA is keyed on.
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// Statuses.
const (
	StatusNew          = "new"
	StatusAcknowledged = "acknowledged"
	StatusInProgress   = "in_progress"
	StatusResolved     = "resolved"
	StatusClosed       = "closed"
)

// transitions is the allowed status graph.
//
// It is a closed graph rather than a free-text field because the status is
// what an SLA and a dashboard are computed from. Allowing any value would
// make both meaningless, and allowing any transition would let a record go
// from closed back to new, which reads to everyone downstream as a new
// incident that never happened.
var transitions = map[string][]string{
	StatusNew:          {StatusAcknowledged, StatusInProgress, StatusResolved, StatusClosed},
	StatusAcknowledged: {StatusInProgress, StatusResolved, StatusClosed},
	StatusInProgress:   {StatusResolved, StatusClosed},
	// Resolved may reopen: a fix that did not hold is the same incident, and
	// opening a second record loses the history of the first.
	StatusResolved: {StatusInProgress, StatusClosed},
	// Closed is terminal. Reopening a closed record would silently rewrite
	// whatever has already been reported from it.
	StatusClosed: {},
}

// Errors the platform distinguishes.
var (
	ErrUnknownKind       = errors.New("unknown support kind")
	ErrUnknownSeverity   = errors.New("unknown severity")
	ErrUnknownStatus     = errors.New("unknown status")
	ErrInvalidTransition = errors.New("invalid status transition")
)

// NormalizeKind validates a kind.
func NormalizeKind(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", KindIncident:
		return KindIncident, nil
	case KindRequest:
		return KindRequest, nil
	default:
		return "", ErrUnknownKind
	}
}

// NormalizeSeverity validates a severity. There is no default: an unstated
// severity is a question nobody has answered, and answering it here would
// pick the response time on the reporter's behalf.
func NormalizeSeverity(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SeverityLow:
		return SeverityLow, nil
	case SeverityMedium:
		return SeverityMedium, nil
	case SeverityHigh:
		return SeverityHigh, nil
	case SeverityCritical:
		return SeverityCritical, nil
	default:
		return "", ErrUnknownSeverity
	}
}

// NormalizeStatus validates a status.
func NormalizeStatus(value string) (string, error) {
	status := strings.ToLower(strings.TrimSpace(value))
	if _, known := transitions[status]; !known {
		return "", ErrUnknownStatus
	}
	return status, nil
}

// CanTransition reports whether a status may move from one value to another.
// Staying put is always allowed, so an update that does not mention the
// status is not a transition.
func CanTransition(from, to string) error {
	if from == to {
		return nil
	}
	allowed, known := transitions[from]
	if !known {
		return ErrUnknownStatus
	}
	for _, candidate := range allowed {
		if candidate == to {
			return nil
		}
	}
	return ErrInvalidTransition
}

// Transitions returns the graph, for documentation and contract tests.
func Transitions() map[string][]string {
	copied := make(map[string][]string, len(transitions))
	for from, to := range transitions {
		copied[from] = append([]string(nil), to...)
	}
	return copied
}

// IsOpen reports whether a record is still being worked.
func IsOpen(status string) bool {
	return status != StatusResolved && status != StatusClosed
}

// SLA states.
const (
	// SLAUnset means no policy is configured for this severity, so the
	// platform makes no claim about when anything is due. It is distinct
	// from "on track" on purpose: a customer reading "on track" has been
	// told a promise is being kept, and there is no promise.
	SLAUnset    = "unset"
	SLAOnTrack  = "on_track"
	SLAAtRisk   = "at_risk"
	SLABreached = "breached"
	// SLAMet means the work finished inside the target.
	SLAMet = "met"
)

// Policy is the SLA target for one severity.
//
// The durations are configuration, not code. Nothing in this repository
// supplies a default: see the package comment.
type Policy struct {
	// Respond is how long there is to acknowledge.
	Respond time.Duration
	// Resolve is how long there is to resolve.
	Resolve time.Duration
}

// SLA is the derived state of one record against its policy.
type SLA struct {
	State string `json:"state"`
	// RespondBy and ResolveBy are absent when no policy is configured.
	RespondBy *time.Time `json:"respond_by,omitempty"`
	ResolveBy *time.Time `json:"resolve_by,omitempty"`
}

// atRiskFraction is how much of the remaining budget must be gone before a
// record is called at risk. It is a display threshold rather than a
// commitment, so unlike the targets themselves it is safe to choose here.
const atRiskFraction = 0.8

// Evaluate derives the SLA state of a record.
//
// acknowledgedAt and resolvedAt are the moments the record reached those
// states, or nil where it has not. Passing the clock in keeps the result a
// function of its inputs, so a test does not have to wait.
func Evaluate(policy Policy, createdAt time.Time, acknowledgedAt, resolvedAt *time.Time, now time.Time) SLA {
	if policy.Respond <= 0 && policy.Resolve <= 0 {
		return SLA{State: SLAUnset}
	}

	sla := SLA{State: SLAOnTrack}
	if policy.Respond > 0 {
		respondBy := createdAt.Add(policy.Respond).UTC()
		sla.RespondBy = &respondBy
	}
	if policy.Resolve > 0 {
		resolveBy := createdAt.Add(policy.Resolve).UTC()
		sla.ResolveBy = &resolveBy
	}

	// A resolved record is judged on when it was resolved, not on the clock:
	// its state must stop moving once the work is done, or a report written a
	// week later would show every closed incident as breached.
	if resolvedAt != nil {
		if sla.ResolveBy != nil && resolvedAt.After(*sla.ResolveBy) {
			return SLA{State: SLABreached, RespondBy: sla.RespondBy, ResolveBy: sla.ResolveBy}
		}
		return SLA{State: SLAMet, RespondBy: sla.RespondBy, ResolveBy: sla.ResolveBy}
	}

	// The response target only binds until it is met. A record acknowledged
	// in time has kept that half of the promise whatever happens next.
	if sla.RespondBy != nil {
		switch {
		case acknowledgedAt == nil && now.After(*sla.RespondBy):
			sla.State = SLABreached
			return sla
		case acknowledgedAt != nil && acknowledgedAt.After(*sla.RespondBy):
			sla.State = SLABreached
			return sla
		}
	}
	if sla.ResolveBy != nil && now.After(*sla.ResolveBy) {
		sla.State = SLABreached
		return sla
	}

	// At risk is the last of the budget, whichever target is nearer.
	for _, due := range []*time.Time{sla.RespondBy, sla.ResolveBy} {
		if due == nil {
			continue
		}
		if due == sla.RespondBy && acknowledgedAt != nil {
			continue
		}
		total := due.Sub(createdAt)
		if total <= 0 {
			continue
		}
		elapsed := now.Sub(createdAt)
		if float64(elapsed) >= float64(total)*atRiskFraction {
			sla.State = SLAAtRisk
		}
	}
	return sla
}

// Relations an incident may carry. The list is closed so a relation cannot
// point at something the platform has no idea how to resolve.
var relationTypes = map[string]bool{
	"client": true, "server": true, "project": true,
	"task": true, "bug": true, "document": true,
}

// IsRelationType reports whether an entity type may be related to a support
// record.
func IsRelationType(entityType string) bool {
	return relationTypes[strings.ToLower(strings.TrimSpace(entityType))]
}

// RelationTypes returns the closed list, for documentation and tests.
func RelationTypes() []string {
	types := make([]string, 0, len(relationTypes))
	for entityType := range relationTypes {
		types = append(types, entityType)
	}
	return types
}

// Record is a support incident or request.
type Record struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Summary  string `json:"summary,omitempty"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	// ClientID is the affected customer, when the record names one. It is
	// also what a scope grant is written against, so a customer can see their
	// own incidents and no others.
	ClientID string `json:"client_id,omitempty"`
	// Relations are Global IDs of everything else the record touches.
	Relations []Relation `json:"relations"`
	// ReportedBy is the Global user ID of whoever raised it.
	ReportedBy     string     `json:"reported_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	SLA            SLA        `json:"sla"`
}

// Relation is one thing a support record points at.
type Relation struct {
	EntityType string `json:"entity_type"`
	GlobalID   string `json:"global_id"`
}
