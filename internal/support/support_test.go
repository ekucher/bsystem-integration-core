package support

import (
	"errors"
	"testing"
	"time"
)

func TestTheStatusGraphIsClosedAndClosedIsTerminal(t *testing.T) {
	// A record that can go from closed back to new reads to everyone
	// downstream as a new incident that never happened, and silently rewrites
	// whatever has already been reported from it.
	for _, to := range []string{StatusNew, StatusAcknowledged, StatusInProgress, StatusResolved} {
		if err := CanTransition(StatusClosed, to); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("closed -> %s was allowed", to)
		}
	}
	// A fix that did not hold is the same incident. Opening a second record
	// would lose the history of the first.
	if err := CanTransition(StatusResolved, StatusInProgress); err != nil {
		t.Errorf("a resolved record cannot be reopened: %v", err)
	}
	// Nothing returns to new: it is the state of a record nobody has looked
	// at, and that stops being true the moment somebody does.
	for from := range Transitions() {
		if from == StatusNew {
			continue
		}
		if err := CanTransition(from, StatusNew); err == nil {
			t.Errorf("%s -> new was allowed", from)
		}
	}
	// Staying put is not a transition, so an update that does not mention the
	// status is never refused.
	for from := range Transitions() {
		if err := CanTransition(from, from); err != nil {
			t.Errorf("%s -> %s was refused: %v", from, from, err)
		}
	}
	if err := CanTransition("nonsense", StatusClosed); !errors.Is(err, ErrUnknownStatus) {
		t.Errorf("an unknown source status gave %v", err)
	}
}

func TestVocabulariesAreClosed(t *testing.T) {
	if _, err := NormalizeSeverity(""); !errors.Is(err, ErrUnknownSeverity) {
		t.Error("an unstated severity was given a default; that picks the response time on the reporter's behalf")
	}
	for _, value := range []string{"sev1", "urgent", "p1", "blocker"} {
		if _, err := NormalizeSeverity(value); err == nil {
			t.Errorf("severity %q was accepted", value)
		}
	}
	for value, want := range map[string]string{
		"LOW": SeverityLow, " medium ": SeverityMedium, "high": SeverityHigh, "critical": SeverityCritical,
	} {
		got, err := NormalizeSeverity(value)
		if err != nil || got != want {
			t.Errorf("NormalizeSeverity(%q) = (%q,%v)", value, got, err)
		}
	}

	// A kind does default, because "incident" is the overwhelmingly common
	// case and choosing it commits nobody to anything.
	if got, err := NormalizeKind(""); err != nil || got != KindIncident {
		t.Errorf("NormalizeKind(\"\") = (%q,%v), want incident", got, err)
	}
	if _, err := NormalizeKind("ticket"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("kind %q was accepted", "ticket")
	}
}

var created = time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)

func at(minutes int) *time.Time {
	moment := created.Add(time.Duration(minutes) * time.Minute)
	return &moment
}

// With no policy configured the platform must make no claim at all. "On
// track" would tell a customer a promise is being kept when there is no
// promise, which is the one answer worse than "unset".
func TestWithNoPolicyThereIsNoClaim(t *testing.T) {
	sla := Evaluate(Policy{}, created, nil, nil, created.Add(72*time.Hour))
	if sla.State != SLAUnset {
		t.Errorf("state = %q, want unset", sla.State)
	}
	if sla.RespondBy != nil || sla.ResolveBy != nil {
		t.Error("due dates were invented without a policy")
	}
}

func TestSLAStates(t *testing.T) {
	policy := Policy{Respond: 60 * time.Minute, Resolve: 240 * time.Minute}

	cases := []struct {
		name           string
		acknowledgedAt *time.Time
		resolvedAt     *time.Time
		nowMinutes     int
		want           string
	}{
		{"fresh", nil, nil, 5, SLAOnTrack},
		{"most of the response budget gone", nil, nil, 50, SLAAtRisk},
		{"response overdue", nil, nil, 61, SLABreached},
		// Acknowledging in time keeps that half of the promise, whatever
		// happens next, so the record is judged on the resolution target only.
		{"acknowledged in time", at(30), nil, 70, SLAOnTrack},
		{"acknowledged in time, resolution nearing", at(30), nil, 200, SLAAtRisk},
		{"acknowledged late is a breach", at(90), nil, 100, SLABreached},
		{"resolution overdue", at(30), nil, 241, SLABreached},
		{"resolved in time", at(30), at(120), 5000, SLAMet},
		{"resolved late", at(30), at(300), 5000, SLABreached},
		// A resolved record's state must stop moving, or a report written a
		// week later shows every closed incident as breached.
		{"a resolved record does not decay", at(30), at(120), 100000, SLAMet},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sla := Evaluate(policy, created, c.acknowledgedAt, c.resolvedAt, created.Add(time.Duration(c.nowMinutes)*time.Minute))
			if sla.State != c.want {
				t.Errorf("state = %q, want %q", sla.State, c.want)
			}
			if sla.RespondBy == nil || sla.ResolveBy == nil {
				t.Error("a configured policy produced no due dates")
			}
		})
	}
}

// A policy may set one target and not the other, and the state must follow
// only the target that exists.
func TestAPartialPolicyBindsOnlyWhatItSets(t *testing.T) {
	respondOnly := Policy{Respond: 60 * time.Minute}
	sla := Evaluate(respondOnly, created, nil, nil, created.Add(10*time.Hour))
	if sla.State != SLABreached || sla.ResolveBy != nil {
		t.Errorf("respond-only policy gave %+v", sla)
	}
	resolveOnly := Policy{Resolve: 240 * time.Minute}
	sla = Evaluate(resolveOnly, created, nil, nil, created.Add(10*time.Minute))
	if sla.State != SLAOnTrack || sla.RespondBy != nil {
		t.Errorf("resolve-only policy gave %+v", sla)
	}
}

func TestRelationTypesAreClosed(t *testing.T) {
	for _, entityType := range RelationTypes() {
		if !IsRelationType(entityType) {
			t.Errorf("%s is listed but not accepted", entityType)
		}
	}
	for _, entityType := range []string{"user", "service", "release", "repository", "", "nonsense"} {
		if IsRelationType(entityType) {
			t.Errorf("%s was accepted as a relation type", entityType)
		}
	}
}

func TestIsOpen(t *testing.T) {
	for _, status := range []string{StatusNew, StatusAcknowledged, StatusInProgress} {
		if !IsOpen(status) {
			t.Errorf("%s should be open", status)
		}
	}
	for _, status := range []string{StatusResolved, StatusClosed} {
		if IsOpen(status) {
			t.Errorf("%s should not be open", status)
		}
	}
}
