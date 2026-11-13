package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSLOs(t *testing.T) {
	s := mustSLOs(t)
	if len(s.Journeys) != 4 || s.Window != "28d" {
		t.Fatalf("spec = %+v", s)
	}
	co := s.Journeys[3]
	if co.ID != "checkout" || co.Availability.Target != 0.99 || co.Latency.ThresholdMs != 1000 || co.Latency.Target != 0.95 {
		t.Errorf("checkout = %+v", co)
	}
}

func TestLoadSLOsErrors(t *testing.T) {
	tests := []struct {
		name, yaml string
		want       []string
	}{
		{"wrong kind", "apiVersion: mamori/v2\nkind: CUJMap\njourneys: []\n", []string{`apiVersion is "mamori/v2"`, `kind is "CUJMap"`, "no journeys"}},
		{"bad values", `kind: SLOSpec
journeys:
  - {id: Browse, availability: {target: 99.5}, latency: {thresholdMs: 0, target: 1}}
  - {id: checkout, availability: {target: 0.99}, latency: {thresholdMs: 1000, target: 0.95}}
  - {id: checkout, availability: {target: 0.99}, latency: {thresholdMs: 1000, target: 0.95}}
`, []string{`journeys[0]: id "Browse"`, "availability target 99.5", "latency target 1 ", "thresholdMs 0", "checkout: listed twice"}},
		{"not yaml", "kind: [", []string{"slos.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "slos.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadSLOs(path)
			for _, want := range tt.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("loadSLOs() error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
	if _, err := loadSLOs(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("want an error for a missing file")
	}
}

func TestThresholdLabelMatchesK6(t *testing.T) {
	// k6 writes String(t) from JavaScript: 500 -> "500", 1000 -> "1000", 250.5 -> "250.5".
	for in, want := range map[float64]string{500: "500", 1000: "1000", 250.5: "250.5"} {
		if got := thresholdLabel(in); got != want {
			t.Errorf("thresholdLabel(%g) = %q, want %q", in, got, want)
		}
	}
}
