// Package observability provides BSYSTEM's structured logging and metrics.
//
// Both exist to answer questions during an incident, which shapes what they
// carry: an identifier that ties a log line to a request and to the audit
// trail, bounded labels that a time series can actually be grouped by, and
// nothing that would turn a log store into a place secrets accumulate.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// Redacted replaces the value of any attribute whose key names a credential.
const Redacted = "[REDACTED]"

// sensitiveKeys are attribute names whose values must never be written.
//
// This is defence in depth rather than the primary control: the primary
// control is that nothing logs a header map, a query string or a request body
// in the first place. Redaction catches the case where someone adds one
// later without thinking about it.
var sensitiveKeys = []string{
	"authorization", "token", "access_token", "refresh_token", "id_token",
	"password", "secret", "api_key", "apikey", "x-api-key", "credential",
	"cookie", "set-cookie", "private_key", "client_secret",
}

// IsSensitive reports whether an attribute key names a credential.
func IsSensitive(key string) bool {
	lower := strings.ToLower(key)
	for _, sensitive := range sensitiveKeys {
		if lower == sensitive || strings.Contains(lower, sensitive) {
			return true
		}
	}
	return false
}

// redact is the slog hook that enforces the rule above on every attribute,
// however deeply nested, and whatever produced it.
func redact(_ []string, attr slog.Attr) slog.Attr {
	if IsSensitive(attr.Key) {
		return slog.String(attr.Key, Redacted)
	}
	return attr
}

// NewLogger returns the platform's JSON logger.
//
// JSON rather than text because these lines are read by a log store far more
// often than by a person, and a person reading one line is not helped much by
// alignment.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redact,
	}))
}

// LevelFromEnv maps a configured level name onto a slog level, defaulting to
// info for anything unrecognised rather than failing to start.
func LevelFromEnv(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type contextKey string

const loggerContextKey contextKey = "logger"

// WithLogger returns a context carrying a logger, so that a handler can log
// with the request's own fields already attached.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey, logger)
}

// LoggerFrom returns the request's logger, or the default one.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerContextKey).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}
