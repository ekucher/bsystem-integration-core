package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func decode(t *testing.T, buffer *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buffer.String())
	}
	return entry
}

func TestLogsAreStructuredJSON(t *testing.T) {
	var buffer bytes.Buffer
	NewLogger(&buffer, slog.LevelInfo).Info("request handled",
		slog.String("request_id", "abc"),
		slog.String("route", "GET /api/v1/me"),
		slog.Int("status", 200),
	)

	entry := decode(t, &buffer)
	for _, key := range []string{"time", "level", "msg", "request_id", "route", "status"} {
		if _, present := entry[key]; !present {
			t.Errorf("log entry is missing %q: %v", key, entry)
		}
	}
	if entry["msg"] != "request handled" {
		t.Fatalf("msg = %v", entry["msg"])
	}
}

// Redaction is defence in depth: the primary control is that nothing logs a
// header, a query string or a body. This catches the case where someone adds
// one later without thinking about it.
func TestSensitiveAttributesAreRedacted(t *testing.T) {
	tests := []struct {
		name string
		attr slog.Attr
	}{
		{name: "authorization header", attr: slog.String("authorization", "Bearer super-secret")},
		{name: "mixed case", attr: slog.String("Authorization", "Bearer super-secret")},
		{name: "access token", attr: slog.String("access_token", "super-secret")},
		{name: "refresh token", attr: slog.String("refresh_token", "super-secret")},
		{name: "api key", attr: slog.String("api_key", "super-secret")},
		{name: "vendor api key header", attr: slog.String("X-Api-Key", "super-secret")},
		{name: "password", attr: slog.String("password", "super-secret")},
		{name: "client secret", attr: slog.String("client_secret", "super-secret")},
		{name: "cookie", attr: slog.String("cookie", "session=super-secret")},
		{name: "private key", attr: slog.String("private_key", "super-secret")},
		{name: "a key that merely contains one", attr: slog.String("upstream_api_key", "super-secret")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			NewLogger(&buffer, slog.LevelInfo).Info("call", test.attr)
			if strings.Contains(buffer.String(), "super-secret") {
				t.Fatalf("credential reached the log: %s", buffer.String())
			}
			if !strings.Contains(buffer.String(), Redacted) {
				t.Fatalf("value was dropped rather than redacted: %s", buffer.String())
			}
		})
	}
}

// Redaction must reach attributes wherever they are, including inside a group
// and on a logger that was built with them already attached.
func TestRedactionReachesNestedAndPreboundAttributes(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewLogger(&buffer, slog.LevelInfo).With(slog.String("token", "super-secret"))
	logger.Info("call", slog.Group("upstream", slog.String("authorization", "Bearer super-secret")))

	if strings.Contains(buffer.String(), "super-secret") {
		t.Fatalf("credential reached the log: %s", buffer.String())
	}
}

func TestOrdinaryAttributesSurvive(t *testing.T) {
	var buffer bytes.Buffer
	NewLogger(&buffer, slog.LevelInfo).Info("call",
		slog.String("route", "GET /api/v1/clients"),
		slog.String("actor", "USR-000001"),
		slog.String("source", "espocrm"),
	)
	entry := decode(t, &buffer)
	if entry["route"] != "GET /api/v1/clients" || entry["actor"] != "USR-000001" || entry["source"] != "espocrm" {
		t.Fatalf("an ordinary attribute was altered: %v", entry)
	}
}

func TestIsSensitive(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{key: "authorization", want: true},
		{key: "AUTHORIZATION", want: true},
		{key: "x-api-key", want: true},
		{key: "route"},
		{key: "status"},
		{key: "duration_ms"},
		{key: "actor"},
		// Matching is by substring, so a field that merely contains a
		// sensitive word is redacted too. That costs the occasional benign
		// field, which is the right side to err on: a redacted field is an
		// inconvenience, a leaked credential is an incident.
		{key: "tokens_processed", want: true},
	}
	for _, test := range tests {
		if got := IsSensitive(test.key); got != test.want {
			t.Errorf("IsSensitive(%q) = %v, want %v", test.key, got, test.want)
		}
	}
}

func TestLevelFromEnv(t *testing.T) {
	tests := []struct {
		value string
		want  slog.Level
	}{
		{value: "debug", want: slog.LevelDebug},
		{value: "DEBUG", want: slog.LevelDebug},
		{value: " warn ", want: slog.LevelWarn},
		{value: "warning", want: slog.LevelWarn},
		{value: "error", want: slog.LevelError},
		{value: "info", want: slog.LevelInfo},
		// An unrecognised level must not stop the service from starting.
		{value: "chatty", want: slog.LevelInfo},
		{value: "", want: slog.LevelInfo},
	}
	for _, test := range tests {
		if got := LevelFromEnv(test.value); got != test.want {
			t.Errorf("LevelFromEnv(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestLevelFiltering(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewLogger(&buffer, slog.LevelWarn)
	logger.Info("ignored")
	if buffer.Len() != 0 {
		t.Fatalf("an info line was written at warn level: %s", buffer.String())
	}
	logger.Warn("kept")
	if !strings.Contains(buffer.String(), "kept") {
		t.Fatalf("a warn line was dropped: %s", buffer.String())
	}
}
