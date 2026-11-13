package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLoggerWritesCloudLoggingFields(t *testing.T) {
	var b bytes.Buffer
	log := newLogger(&b)
	log.Warn("context query failed", "what", "nodes")
	log.WithGroup("g").Info("grouped", "level", "kept")
	log.Error("report failed")

	lines := bytes.Split(bytes.TrimSpace(b.Bytes()), []byte("\n"))
	want := []string{"WARNING", "INFO", "ERROR"}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("line %d not JSON: %s", i, line)
		}
		if m["severity"] != want[i] || m["message"] == nil {
			t.Errorf("line %d = %v", i, m)
		}
	}
	if got := severity(slog.LevelDebug); got != "DEBUG" {
		t.Errorf("severity(debug) = %s", got)
	}
}
