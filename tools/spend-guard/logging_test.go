package spendguard

import (
	"log/slog"
	"testing"
)

func TestLoggerUsesCloudLoggingFields(t *testing.T) {
	logs := &logBuffer{}
	log := newLogger(logs)
	log.Debug("hidden")
	log.Info("info", "k", "v")
	log.Warn("warn")
	log.Error("error")

	entries := logs.entries(t)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (debug is below the level)", len(entries))
	}
	for i, want := range []string{"INFO", "WARNING", "ERROR"} {
		e := entries[i]
		if e["severity"] != want {
			t.Errorf("entry %d severity = %v, want %v", i, e["severity"], want)
		}
		if _, ok := e["message"]; !ok {
			t.Errorf("entry %d has no message field: %v", i, e)
		}
		if _, ok := e["level"]; ok {
			t.Errorf("entry %d still has slog's level field", i)
		}
		if _, ok := e["msg"]; ok {
			t.Errorf("entry %d still has slog's msg field", i)
		}
	}
	if entries[0]["k"] != "v" {
		t.Errorf("attribute k = %v, want v", entries[0]["k"])
	}
}

func TestLoggerLeavesGroupedKeysAlone(t *testing.T) {
	logs := &logBuffer{}
	newLogger(logs).Info("grouped", slog.Group("op", "level", "x", "msg", "y"))

	group, ok := logs.entries(t)[0]["op"].(map[string]any)
	if !ok || group["level"] != "x" || group["msg"] != "y" {
		t.Errorf("group = %v, want keys unchanged", group)
	}
}

func TestSeverity(t *testing.T) {
	tests := map[slog.Level]string{
		slog.LevelDebug:     "DEBUG",
		slog.LevelInfo:      "INFO",
		slog.LevelInfo + 1:  "INFO",
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
