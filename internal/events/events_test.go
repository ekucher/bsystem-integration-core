package events

import (
	"testing"
	"time"
)

func TestEnvelopeNormalize(t *testing.T) {
	now := time.Date(2026, 9, 15, 20, 0, 0, 0, time.UTC)
	e := Envelope{Event: " backup.failed ", Source: " operations ", Severity: "ERROR"}
	if err := e.Normalize(now); err != nil {
		t.Fatalf("normalize event: %v", err)
	}
	if e.Event != "backup.failed" || e.Source != "operations" || e.Severity != "error" {
		t.Fatalf("unexpected normalized envelope: %#v", e)
	}
	if !e.OccurredAt.Equal(now) {
		t.Fatalf("unexpected occurred_at: %v", e.OccurredAt)
	}
	if Subject(e.Event) != "bsystem.events.backup.failed" {
		t.Fatalf("unexpected subject: %s", Subject(e.Event))
	}
}

func TestEnvelopeRejectsInvalidEvent(t *testing.T) {
	e := Envelope{Event: "invalid", Source: "test"}
	if err := e.Normalize(time.Now()); err == nil {
		t.Fatal("expected invalid event name to fail")
	}
}
