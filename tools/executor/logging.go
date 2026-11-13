package main

import (
	"io"
	"log/slog"
)

// newLogger writes one JSON object per line. Cloud Logging reads "severity"
// as the log level and "message" as the summary line; the other fields land
// in jsonPayload, where the runbook queries them (jsonPayload.event, ...).
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: cloudLoggingAttr,
	}))
}

func cloudLoggingAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		level, _ := a.Value.Any().(slog.Level)
		return slog.String("severity", severity(level))
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

// severity maps slog levels to Cloud Logging's LogSeverity names.
func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}
