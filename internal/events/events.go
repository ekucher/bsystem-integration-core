package events

import (
	"errors"
	"strings"
	"time"
)

type Envelope struct {
	Event      string         `json:"event"`
	Source     string         `json:"source"`
	ActorID    string         `json:"actor_id,omitempty"`
	EntityID   string         `json:"entity_id,omitempty"`
	TenantID   string         `json:"tenant_id,omitempty"`
	Severity   string         `json:"severity,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
	RequestID  string         `json:"request_id,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
}

func (e *Envelope) Normalize(now time.Time) error {
	e.Event = strings.TrimSpace(e.Event)
	e.Source = strings.TrimSpace(e.Source)
	e.ActorID = strings.TrimSpace(e.ActorID)
	e.EntityID = strings.TrimSpace(e.EntityID)
	e.TenantID = strings.TrimSpace(e.TenantID)
	e.Severity = strings.TrimSpace(strings.ToLower(e.Severity))
	if e.Event == "" || e.Source == "" {
		return errors.New("event and source are required")
	}
	parts := strings.Split(e.Event, ".")
	if len(parts) < 2 {
		return errors.New("event must use entity.action naming")
	}
	for _, part := range parts {
		if part == "" {
			return errors.New("event contains an empty name segment")
		}
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = now.UTC()
	} else {
		e.OccurredAt = e.OccurredAt.UTC()
	}
	if e.Severity == "" {
		e.Severity = "info"
	}
	switch e.Severity {
	case "debug", "info", "warning", "error", "critical":
	default:
		return errors.New("unsupported severity")
	}
	return nil
}

func Subject(event string) string {
	return "bsystem.events." + event
}
