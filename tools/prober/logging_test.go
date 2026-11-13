package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLoggerWritesCloudLoggingFields(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf, slog.LevelDebug)
	log.Warn("probe failed", "cuj", "browse", slog.Group("steps_ms", slog.Int64("home", 12)))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON object: %q", buf.String())
	}
	if line["severity"] != "WARNING" || line["message"] != "probe failed" || line["cuj"] != "browse" {
		t.Errorf("line = %v", line)
	}
	if steps, _ := line["steps_ms"].(map[string]any); steps["home"] != float64(12) {
		t.Errorf("steps_ms = %v", line["steps_ms"])
	}
	if _, ok := line["level"]; ok {
		t.Error("line still has a level field")
	}
}

func TestSeverity(t *testing.T) {
	tests := map[slog.Level]string{
		slog.LevelDebug:     "DEBUG",
		slog.LevelInfo:      "INFO",
		slog.LevelWarn:      "WARNING",
		slog.LevelError:     "ERROR",
		slog.LevelError + 4: "ERROR",
	}
	for level, want := range tests {
		if got := severity(level); got != want {
			t.Errorf("severity(%v) = %q, want %q", level, got, want)
		}
	}
}

func TestLoggerLevel(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, slog.LevelInfo).Debug("hidden")
	if buf.Len() != 0 {
		t.Errorf("debug line written at info level: %q", buf.String())
	}
}
