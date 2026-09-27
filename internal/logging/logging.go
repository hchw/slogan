// Package logging provides a structured JSON logger with defensive redaction
// for sensitive fields. Callers should still avoid logging request bodies or
// credentials at all.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON slog.Logger configured for the given environment.
func New(env string) *slog.Logger { return NewWithWriter(env, os.Stdout) }

// NewWithWriter is exposed for deterministic tests and alternate sinks.
func NewWithWriter(env string, output io.Writer) *slog.Logger {
	level := slog.LevelInfo
	if strings.EqualFold(env, "development") || strings.EqualFold(env, "debug") {
		level = slog.LevelDebug
	}
	h := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redact,
	})
	return slog.New(h).With("service", "slogan")
}

func redact(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(strings.ReplaceAll(a.Key, "-", "_"))
	for _, sensitive := range []string{"secret", "password", "authorization", "api_key", "token", "prompt", "request_body", "response_body", "content"} {
		if strings.Contains(key, sensitive) {
			a.Value = slog.StringValue("[REDACTED]")
			return a
		}
	}
	return a
}
