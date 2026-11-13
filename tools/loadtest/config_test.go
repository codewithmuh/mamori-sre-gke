package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg := mustConfig(t)
	if cfg.Cluster != "mamori" || cfg.Warmup.Duration != time.Minute || cfg.MinDeliveredRatio != 0.95 || cfg.ConfidenceZ != 1.645 {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Metrics.Requests != "k6_mamori_cuj_requests_total" || cfg.Metrics.Duration != "k6_http_req_duration_milliseconds" {
		t.Errorf("metric names = %+v", cfg.Metrics)
	}
	// The price the plan and the research doc use: Spot e2-standard-2 in Tokyo.
	if cfg.Cost.NodeHourly != 0.051584 || cfg.Cost.Nodes != 3 || cfg.Cost.MachineType != "e2-standard-2" {
		t.Errorf("cost = %+v", cfg.Cost)
	}
	for _, q := range []string{cfg.Context.GeneratorCPU, cfg.Context.NodeCPU, cfg.Context.Nodes, cfg.Context.FrontendRPS} {
		if !strings.Contains(q, "${range}") {
			t.Errorf("context query %q has no ${range}", q)
		}
	}
}

func TestLoadConfigOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	data := "warmup: 90s\ncost:\n  nodeHourly: 0.1\n  nodes: 4\ncontext:\n  frontendRPS: \"\"\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Warmup.Duration != 90*time.Second || cfg.Cost.NodeHourly != 0.1 || cfg.Cost.Nodes != 4 || cfg.Context.FrontendRPS != "" {
		t.Errorf("override = %+v", cfg)
	}
	// Keys left out keep the built-in values.
	if cfg.Cost.DiskGBPerNode != 30 || cfg.Context.NodeCPU == "" || cfg.Metrics.Good == "" {
		t.Errorf("defaults lost: %+v", cfg)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown key", "warmpu: 60s\n", "field warmpu not found"},
		{"bad duration", "warmup: soon\n", "invalid duration"},
		{"duration not a string", "warmup: [1]\n", "cannot unmarshal"},
		{"fractional warmup", "warmup: 1500ms\n", "whole seconds"},
		{"bad cluster", "cluster: Mamori\n", `cluster "Mamori"`},
		{"ratio", "minDeliveredRatio: 1.5\n", "minDeliveredRatio 1.5"},
		{"min requests", "minRequestsPerJourney: 0\n", "minRequestsPerJourney 0"},
		{"cpu limit", "generatorCPULimit: 0\n", "generatorCPULimit 0"},
		{"rate limit share", "rateLimitShare: 1.2\n", "rateLimitShare 1.2"},
		{"confidence", "confidenceZ: 0\n", "confidenceZ 0"},
		{"status label", "metrics:\n  statusLabel: \"\"\n", `metrics.statusLabel ""`},
		{"http requests metric", "metrics:\n  httpRequests: \"a-b\"\n", `metrics.httpRequests "a-b"`},
		{"metric name", "metrics:\n  good: \"k6 good\"\n", `metrics.good "k6 good"`},
		{"label name", "metrics:\n  runLabel: 1run\n", `metrics.runLabel "1run"`},
		{"cost", "cost:\n  currency: \"\"\n  nodes: 0\n  nodeHourly: -1\n  diskGBMonthly: -1\n  hoursPerMonth: 0\n  allocatableCPUPerNode: 0\n",
			"cost.currency is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("loadConfig() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadConfigReportsAllCostProblems(t *testing.T) {
	cfg := mustConfig(t)
	cfg.Cost = costModel{}
	err := cfg.validate()
	for _, want := range []string{"cost.currency", "cost.nodes", "cost.nodeHourly", "cost.hoursPerMonth", "cost.allocatableCPUPerNode"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validate() = %v, want %s", err, want)
		}
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestDurationJSON(t *testing.T) {
	b, err := json.Marshal(duration{90 * time.Second})
	if err != nil || string(b) != `"1m30s"` {
		t.Errorf("json = %s, %v", b, err)
	}
}
