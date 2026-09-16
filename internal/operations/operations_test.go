package operations

import "testing"

// The vocabulary is closed. An operations feed that accepts any name becomes
// a log, and nobody can write a query or a notification rule against a log
// whose contents are unbounded.
func TestTheVocabularyIsClosed(t *testing.T) {
	for _, name := range []string{"backup.done", "server.down", "disk.full", "", "  ", "BACKUP.FAILED"} {
		if _, err := Lookup(name); err == nil {
			t.Errorf("%q was accepted but is not in the vocabulary", name)
		}
	}
	for name := range Events() {
		if _, err := Lookup(name); err != nil {
			t.Errorf("%q is in the vocabulary but was refused: %v", name, err)
		}
	}
}

// This is the distinction the module turns on. A failed backup is a serious
// event about a service the server runs; recording it as "the server is in
// error" would put a healthy machine on a dashboard as broken and send
// somebody to look at the wrong thing.
func TestOnlyEventsAboutTheServerItselfMoveItsStatus(t *testing.T) {
	cases := map[string]string{
		"backup.succeeded":      "",
		"backup.failed":         "",
		"selftest.succeeded":    "",
		"selftest.failed":       "",
		"maintenance.started":   StatusMaintenance,
		"maintenance.completed": StatusOK,
		"server.ok":             StatusOK,
		"server.warning":        StatusWarning,
		"server.error":          StatusError,
		"server.offline":        StatusOffline,
	}
	events := Events()
	if len(events) != len(cases) {
		t.Fatalf("the vocabulary has %d events and this test names %d; a new event needs a decision about status here",
			len(events), len(cases))
	}
	for name, wantStatus := range cases {
		t.Run(name, func(t *testing.T) {
			event, err := Lookup(name)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if event.Status != wantStatus {
				t.Errorf("status = %q, want %q", event.Status, wantStatus)
			}
		})
	}
}

func TestSeveritiesMatchWhatTheEventMeans(t *testing.T) {
	cases := map[string]string{
		"backup.succeeded":      "info",
		"backup.failed":         "critical",
		"selftest.succeeded":    "info",
		"selftest.failed":       "error",
		"maintenance.started":   "info",
		"maintenance.completed": "info",
		"server.ok":             "info",
		"server.warning":        "warning",
		"server.error":          "error",
		"server.offline":        "critical",
	}
	for name, want := range cases {
		event, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup %q: %v", name, err)
		}
		if event.Severity != want {
			t.Errorf("%s severity = %q, want %q", name, event.Severity, want)
		}
	}
}

func TestReportersCanEscalateSeverityButNotLowerIt(t *testing.T) {
	cases := []struct {
		name     string
		floor    string
		reported string
		want     string
	}{
		{"a quieter report is ignored", "critical", "info", "critical"},
		{"a louder report is taken", "info", "error", "error"},
		{"an absent report keeps the floor", "error", "", "error"},
		{"an unrecognised report is not trusted", "warning", "apocalyptic", "warning"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EscalateTo(c.floor, c.reported); got != c.want {
				t.Errorf("EscalateTo(%q,%q) = %q, want %q", c.floor, c.reported, got, c.want)
			}
		})
	}
}

// A reporter that does not say which environment a server is in must not have
// one assumed for it, in either direction: recording an unlabelled host as
// production creates false alarms, and recording it as a lab hides real ones.
func TestAnUnstatedEnvironmentIsUnknownRatherThanGuessed(t *testing.T) {
	for _, value := range []string{"", "   ", "unknown"} {
		got, err := NormalizeEnvironment(value)
		if err != nil || got != EnvUnknown {
			t.Errorf("NormalizeEnvironment(%q) = (%q,%v), want unknown", value, got, err)
		}
	}
	for value, want := range map[string]string{
		"dev": EnvDev, "development": EnvDev, "DEV": EnvDev,
		"stage": EnvStage, "staging": EnvStage,
		"prod": EnvProd, "production": EnvProd, " Production ": EnvProd,
	} {
		got, err := NormalizeEnvironment(value)
		if err != nil || got != want {
			t.Errorf("NormalizeEnvironment(%q) = (%q,%v), want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"qa", "preprod", "live", "test"} {
		if _, err := NormalizeEnvironment(value); err == nil {
			t.Errorf("NormalizeEnvironment(%q) was accepted; an unrecognised environment must be refused rather than guessed", value)
		}
	}
}
