package main

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func testEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(testEnv(nil))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if got := cfg.targetURL.String(); got != "http://istio-ingressgateway.istio-ingress.svc.cluster.local" {
		t.Errorf("targetURL = %q", got)
	}
	if cfg.host != "mamori.codewithmuh.com" {
		t.Errorf("host = %q", cfg.host)
	}
	if cfg.interval != 60*time.Second || cfg.stepTimeout != 12*time.Second {
		t.Errorf("interval = %v, stepTimeout = %v", cfg.interval, cfg.stepTimeout)
	}
	if !slices.Equal(cfg.scheduled, []string{"browse", "add-to-cart", "view-cart", "checkout"}) {
		t.Errorf("scheduled = %v", cfg.scheduled)
	}
	if cfg.listenAddr != ":8080" || cfg.runConcurrency != 4 || cfg.logLevel != slog.LevelInfo {
		t.Errorf("listenAddr = %q, runConcurrency = %d, logLevel = %v", cfg.listenAddr, cfg.runConcurrency, cfg.logLevel)
	}
}

func TestLoadConfigValues(t *testing.T) {
	cfg, err := loadConfig(testEnv(map[string]string{
		"PROBER_TARGET_URL":      " https://Mamori.codewithmuh.com/shop/ ",
		"PROBER_HOST":            "Mamori.CodeWithMuh.com:443",
		"PROBER_INTERVAL":        "30s",
		"PROBER_STEP_TIMEOUT":    "5s",
		"PROBER_CUJS":            " checkout , browse ",
		"PROBER_LISTEN_ADDR":     "127.0.0.1:9090",
		"PROBER_RUN_CONCURRENCY": "10",
		"PROBER_LOG_LEVEL":       "debug",
	}))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if got := cfg.targetURL.String(); got != "https://Mamori.codewithmuh.com/shop" {
		t.Errorf("targetURL = %q, want the trailing slash removed", got)
	}
	if cfg.host != "mamori.codewithmuh.com:443" {
		t.Errorf("host = %q", cfg.host)
	}
	if cfg.interval != 30*time.Second || cfg.stepTimeout != 5*time.Second {
		t.Errorf("interval = %v, stepTimeout = %v", cfg.interval, cfg.stepTimeout)
	}
	if !slices.Equal(cfg.scheduled, []string{"checkout", "browse"}) {
		t.Errorf("scheduled = %v", cfg.scheduled)
	}
	if cfg.listenAddr != "127.0.0.1:9090" || cfg.runConcurrency != 10 || cfg.logLevel != slog.LevelDebug {
		t.Errorf("listenAddr = %q, runConcurrency = %d, logLevel = %v", cfg.listenAddr, cfg.runConcurrency, cfg.logLevel)
	}
}

func TestLoadConfigNoSchedule(t *testing.T) {
	cfg, err := loadConfig(testEnv(map[string]string{"PROBER_CUJS": "None"}))
	if err != nil || len(cfg.scheduled) != 0 || cfg.scheduled == nil {
		t.Errorf("scheduled = %#v, err = %v; want an empty schedule", cfg.scheduled, err)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		key, value, wantErr string
	}{
		{"PROBER_TARGET_URL", "://bad", "is not a URL"},
		{"PROBER_TARGET_URL", "ftp://gateway", "must be an http or https URL"},
		{"PROBER_TARGET_URL", "gateway:80", "must be an http or https URL"},
		{"PROBER_TARGET_URL", "http://user:pw@gateway", "must not have user info"},
		{"PROBER_TARGET_URL", "http://gateway/?a=b", "must not have user info, a query"},
		{"PROBER_HOST", "mamori_shop", "is not a hostname"},
		{"PROBER_HOST", "http://mamori.codewithmuh.com", "is not a hostname"},
		{"PROBER_INTERVAL", "60", "must be a duration from 1s to 1h0m0s"},
		{"PROBER_INTERVAL", "500ms", "must be a duration from 1s"},
		{"PROBER_INTERVAL", "2h", "must be a duration from 1s"},
		{"PROBER_STEP_TIMEOUT", "10ms", "PROBER_STEP_TIMEOUT=\"10ms\" must be a duration from 100ms to 5m0s"},
		{"PROBER_STEP_TIMEOUT", "2m", "must not be longer than PROBER_INTERVAL"},
		{"PROBER_CUJS", "browse,search", `unknown journey "search"`},
		{"PROBER_CUJS", "browse,,checkout", `unknown journey ""`},
		{"PROBER_CUJS", "browse,browse", `"browse" is listed twice`},
		{"PROBER_LISTEN_ADDR", "8080", "must be host:port"},
		{"PROBER_RUN_CONCURRENCY", "0", "must be a number from 1 to 10"},
		{"PROBER_RUN_CONCURRENCY", "11", "must be a number from 1 to 10"},
		{"PROBER_RUN_CONCURRENCY", "four", "must be a number from 1 to 10"},
		{"PROBER_LOG_LEVEL", "verbose", "must be debug, info, warn or error"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			_, err := loadConfig(testEnv(map[string]string{tt.key: tt.value}))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("loadConfig() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigReportsEveryProblem(t *testing.T) {
	cfg, err := loadConfig(testEnv(map[string]string{
		"PROBER_HOST":      "bad host",
		"PROBER_INTERVAL":  "soon",
		"PROBER_LOG_LEVEL": "loud",
	}))
	if err == nil {
		t.Fatal("loadConfig() error = nil")
	}
	for _, key := range []string{"PROBER_HOST", "PROBER_INTERVAL", "PROBER_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
	if cfg.logLevel != slog.LevelInfo {
		t.Errorf("logLevel = %v, want info so the error can be logged", cfg.logLevel)
	}
}
