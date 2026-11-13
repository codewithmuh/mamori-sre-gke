package spendguard

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg := testConfig(t, nil)

	if cfg.Threshold != 1.0 {
		t.Errorf("Threshold = %v, want default 1.0", cfg.Threshold)
	}
	if cfg.DryRun {
		t.Error("DryRun = true, want default false")
	}
	if cfg.MaxMessageAge != time.Hour {
		t.Errorf("MaxMessageAge = %v, want default 1h", cfg.MaxMessageAge)
	}
	if got := cfg.nodePoolName(); got != testNodePool {
		t.Errorf("nodePoolName() = %q, want %q", got, testNodePool)
	}
	if got := fmt.Sprint(cfg.logAttrs()); !strings.Contains(got, testNodePool) {
		t.Errorf("logAttrs() = %s, want the node pool name", got)
	}
	want := "projects/mamori-lab-123456/locations/asia-northeast1-a/operations/operation-7"
	if got := cfg.operationName("operation-7"); got != want {
		t.Errorf("operationName() = %q, want %q", got, want)
	}
}

func TestLoadConfigValues(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"THRESHOLD": " 0.9 ",
		"DRY_RUN":   "TRUE",
		"LOCATION":  "asia-northeast1",

		"MAX_MESSAGE_AGE": "90m",
	})
	if cfg.MaxMessageAge != 90*time.Minute {
		t.Errorf("MaxMessageAge = %v, want 90m", cfg.MaxMessageAge)
	}
	if cfg.Threshold != 0.9 {
		t.Errorf("Threshold = %v, want 0.9", cfg.Threshold)
	}
	if !cfg.DryRun {
		t.Error("DryRun = false, want true")
	}
	if cfg.Location != "asia-northeast1" {
		t.Errorf("Location = %q, want a region to be accepted too", cfg.Location)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"missing project", map[string]string{"PROJECT_ID": ""}, "PROJECT_ID is required"},
		{"missing location", map[string]string{"LOCATION": " "}, "LOCATION is required"},
		{"missing cluster", map[string]string{"CLUSTER_NAME": ""}, "CLUSTER_NAME is required"},
		{"missing node pool", map[string]string{"NODE_POOL": ""}, "NODE_POOL is required"},
		{"bad project", map[string]string{"PROJECT_ID": "Mamori_Lab"}, "PROJECT_ID="},
		{"path in project", map[string]string{"PROJECT_ID": "a/b/c/d/e/f"}, "PROJECT_ID="},
		{"bad location", map[string]string{"LOCATION": "tokyo"}, "LOCATION="},
		{"bad cluster", map[string]string{"CLUSTER_NAME": "Mamori"}, "CLUSTER_NAME="},
		{"bad node pool", map[string]string{"NODE_POOL": "spot-pool/x"}, "NODE_POOL="},
		{"threshold not a number", map[string]string{"THRESHOLD": "one"}, "THRESHOLD=\"one\" is not a number"},
		{"threshold zero", map[string]string{"THRESHOLD": "0"}, "must be a fraction"},
		{"threshold negative", map[string]string{"THRESHOLD": "-1"}, "must be a fraction"},
		{"threshold NaN", map[string]string{"THRESHOLD": "NaN"}, "must be a fraction"},
		{"threshold Inf", map[string]string{"THRESHOLD": "Inf"}, "must be a fraction"},
		{"threshold as percent", map[string]string{"THRESHOLD": "100"}, "must be a fraction"},
		{"dry run not a bool", map[string]string{"DRY_RUN": "yes"}, "DRY_RUN=\"yes\" must be true or false"},
		{"max age not a duration", map[string]string{"MAX_MESSAGE_AGE": "60"}, "MAX_MESSAGE_AGE=\"60\" is not a duration"},
		{"max age too short", map[string]string{"MAX_MESSAGE_AGE": "30s"}, "must be between 5m0s and 24h0m0s"},
		{"max age too long", map[string]string{"MAX_MESSAGE_AGE": "168h"}, "must be between"},
		{"max age negative", map[string]string{"MAX_MESSAGE_AGE": "-1h"}, "must be between"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(testEnv(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadConfig() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigReportsEveryProblem(t *testing.T) {
	_, err := loadConfig(func(string) string { return "" })
	if err == nil {
		t.Fatal("loadConfig() with no environment succeeded")
	}
	for _, key := range []string{"PROJECT_ID", "LOCATION", "CLUSTER_NAME", "NODE_POOL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
}

func TestThresholdUpperBoundAccepted(t *testing.T) {
	if cfg := testConfig(t, map[string]string{"THRESHOLD": "2"}); cfg.Threshold != 2 {
		t.Errorf("Threshold = %v, want 2", cfg.Threshold)
	}
}
