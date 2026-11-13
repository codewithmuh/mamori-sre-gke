package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadK6Summary(t *testing.T) {
	// testdata/k6-summary.json is a real k6 v2.3.0 summary of deploy/loadtest/scripts/capacity.js,
	// trimmed, with the schedule of the fixture run.
	p, err := readK6Summary("testdata/k6-summary.json")
	if err != nil {
		t.Fatal(err)
	}
	if p.RunID != fixtureRunID || !p.Start.Equal(fixtureStart) || p.StepDuration != fixtureStepDuration {
		t.Errorf("plan = %+v", p)
	}
	if !reflect.DeepEqual(p.Steps, fixtureSteps) {
		t.Errorf("steps = %v", p.Steps)
	}
	if want := fixtureStart.Add(1215000500 * time.Microsecond); !p.End.Equal(want) {
		t.Errorf("end = %s, want %s", p.End, want)
	}
	if !reflect.DeepEqual(p.LatencyThresholdsMs, []float64{100, 250, 500, 750, 1000, 1500, 2000, 3000}) {
		t.Errorf("thresholds = %v", p.LatencyThresholdsMs)
	}
}

func TestReadK6SummaryErrors(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"not json", "MAMORI_K6_SUMMARY_BEGIN", "not a k6 summary"},
		{"default summary", `{"metrics": {}, "state": {"testRunDurationMs": 10}}`, "no run plan in setup_data"},
		{"no steps", `{"setup_data": {"start": "2027-01-12T10:00:00Z", "stepSeconds": 300}, "state": {"testRunDurationMs": 10}}`, "no run plan"},
		{"no duration", `{"setup_data": {"start": "2027-01-12T10:00:00Z", "steps": [5], "stepSeconds": 300}, "state": {}}`, "testRunDurationMs"},
	}
	for _, tt := range tests {
		path := filepath.Join(t.TempDir(), "s.json")
		if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readK6Summary(path); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", tt.name, err, tt.want)
		}
	}
	if _, err := readK6Summary(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("want an error for a missing file")
	}
}
