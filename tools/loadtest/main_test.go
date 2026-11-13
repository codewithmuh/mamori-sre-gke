package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunReportFromSummary(t *testing.T) {
	srv := newFakeProm(t).serve(t)
	code, out, errOut := runCLI(t, "report", "--prometheus-url", srv.URL, "--summary", "testdata/k6-summary.json", "--slos", "testdata/slos.yaml")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	for _, want := range []string{
		"# Capacity report: run `20270112-100000`",
		"| Highest rate that held every SLO | **10.0 req/s** (step 2, planned 10 RPS) |",
		"| First step that did not pass | step 3 (20 RPS), fail: checkout: 90.0 % of requests under 1000 ms < 95.0 % |",
		"| Cluster cost (nodes and disks, list price) | $0.1612 an hour, $118 a month |",
		"| Cost per 1K RPS | **$16.12 an hour**, $11,765 a month |",
		"k6 used 0.09 of 5.79 cores, 1.6 %",
		"| checkout | 99.0 % | 95.0 % under 1000 ms |",
		"| 2 | 10 | 10.00 | 100 % | 99.925 % · 99.0 % · 260 ms |",
		"**99.000 % · 90.0 % · 1380 ms**",
		"| 0.85 | 97 % | 4 | 32.0 | fail |",
		"- Step 4: k6 dropped 120 journeys",
		"- Step 4: 30 requests got 429 Too Many Requests",
		"| Suggested ingress gateway rate limit | 9 req/s (90 % of the capacity): `tokens_per_fill`",
		"the Prometheus API at " + srv.URL,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, `"message":"evaluating run"`) || !strings.Contains(errOut, `"severity":"INFO"`) {
		t.Errorf("stderr lacks the JSON log: %s", errOut)
	}
}

func TestRunReportJSONWithFlagsToFile(t *testing.T) {
	srv := newFakeProm(t).serve(t)
	dir := t.TempDir()
	outFile, copyFile := filepath.Join(dir, "report.json"), filepath.Join(dir, "copy.json")
	code, stdout, errOut := runCLI(t, "report", "--prometheus-url", srv.URL, "--slos", "testdata/slos.yaml",
		"--from", "2027-01-12T10:00:00.25Z", "--to", "2027-01-12T10:16:00Z", "--steps", "5,10,20,40",
		"--step-duration", "5m", "--run-id", fixtureRunID, "--format", "json", "--out", outFile, "--json-out", copyFile)
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr:\n%s", code, stdout, errOut)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := os.ReadFile(copyFile); err != nil || !bytes.Equal(again, data) {
		t.Errorf("--json-out copy differs: %v", err)
	}
	var r struct {
		MaxStep             int     `json:"maxStep"`
		FirstNotPassingStep int     `json:"firstNotPassingStep"`
		MaxRPS              float64 `json:"maxRps"`
		SuggestedRateLimit  float64 `json:"suggestedRateLimitRps"`
		StepDuration        string  `json:"stepDuration"`
		Steps               []struct {
			Status string `json:"status"`
		} `json:"steps"`
		Cost struct {
			Per1KRPSHourly float64 `json:"per1kRpsHourly"`
		} `json:"cost"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	// The range ends during step 4, so step 4 is not run.
	if r.MaxStep != 2 || r.FirstNotPassingStep != 3 || r.MaxRPS != 10 || r.SuggestedRateLimit != 9 || r.StepDuration != "5m0s" || r.Steps[3].Status != statusNotRun {
		t.Errorf("report = %+v", r)
	}
	if !near(r.Cost.Per1KRPSHourly, planHourly*1000/10) {
		t.Errorf("cost = %v", r.Cost.Per1KRPSHourly)
	}
}

func TestRunUsageAndErrors(t *testing.T) {
	srv := newFakeProm(t).serve(t)
	base := []string{"report", "--prometheus-url", srv.URL, "--slos", "testdata/slos.yaml"}
	tests := []struct {
		name     string
		args     []string
		code     int
		inStderr string
	}{
		{"no args", nil, 2, "Usage: loadtest report"},
		{"unknown command", []string{"plan"}, 2, `unknown command "plan"`},
		{"bad flag", []string{"report", "--nope"}, 2, "flag provided but not defined"},
		{"extra args", append(append([]string{}, base...), "extra"), 2, "unexpected arguments"},
		{"no source", []string{"report"}, 2, "exactly one of --project or --prometheus-url"},
		{"two sources", append(append([]string{}, base...), "--project", "mamori-lab-123456"), 2, "exactly one"},
		{"bad format", append(append([]string{}, base...), "--format", "csv"), 2, `--format "csv"`},
		{"no schedule", base, 1, "no start time"},
		{"bad from", append(append([]string{}, base...), "--from", "yesterday"), 1, "--from"},
		{"bad to", append(append([]string{}, base...), "--to", "later"), 1, "--to"},
		{"bad steps", append(append([]string{}, base...), "--steps", "5,1"), 1, "higher than the one before"},
		{"bad run id", append(append([]string{}, base...), "--summary", "testdata/k6-summary.json", "--run-id", `x"}`), 1, "may only hold"},
		{"range backwards", append(append([]string{}, base...), "--summary", "testdata/k6-summary.json", "--to", "2027-01-01T00:00:00Z"), 1, "ends"},
		{"missing slos", []string{"report", "--prometheus-url", srv.URL, "--slos", "nope.yaml"}, 1, "nope.yaml"},
		{"missing config", append(append([]string{}, base...), "--config", "nope.yaml"), 1, "nope.yaml"},
		{"missing summary", append(append([]string{}, base...), "--summary", "nope.json"), 1, "nope.json"},
		{"bad url", []string{"report", "--prometheus-url", "localhost", "--slos", "testdata/slos.yaml", "--summary", "testdata/k6-summary.json"}, 1, "must be http"},
		{"short steps", append(append([]string{}, base...), "--summary", "testdata/k6-summary.json", "--step-duration", "90s"), 1, "leaves less than"},
		{"unwritable out", append(append([]string{}, base...), "--summary", "testdata/k6-summary.json", "--out", "/nonexistent/dir/r.md"), 1, "nonexistent"},
		{"unwritable json copy", append(append([]string{}, base...), "--summary", "testdata/k6-summary.json", "--json-out", "/nonexistent/dir/r.json"), 1, "nonexistent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := runCLI(t, tt.args...)
			if code != tt.code || !strings.Contains(errOut, tt.inStderr) {
				t.Errorf("exit %d (want %d), stderr:\n%s\nwant it to contain %q", code, tt.code, errOut, tt.inStderr)
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"report", "-h"}} {
		code, out, errOut := runCLI(t, args...)
		if code != 0 || !strings.Contains(out+errOut, "-step-duration") {
			t.Errorf("%v: exit %d, output %q", args, code, out+errOut)
		}
	}
}

func TestRunStopsWhenTheRunMissedAThreshold(t *testing.T) {
	srv := newFakeProm(t).serve(t)
	slos := filepath.Join(t.TempDir(), "slos.yaml")
	data := "kind: SLOSpec\njourneys:\n  - {id: browse, availability: {target: 0.995}, latency: {thresholdMs: 400, target: 0.95}}\n"
	if err := os.WriteFile(slos, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "report", "--prometheus-url", srv.URL, "--summary", "testdata/k6-summary.json", "--slos", slos)
	if code != 1 || !strings.Contains(errOut, "the SLO's threshold is 400 ms") {
		t.Errorf("exit %d, stderr %s", code, errOut)
	}
}

func TestRunReportQueryFailure(t *testing.T) {
	fake := newFakeProm(t)
	fake.failOn["k6_mamori_cuj_requests_total"] = context.DeadlineExceeded
	srv := fake.serve(t)
	code, _, errOut := runCLI(t, "report", "--prometheus-url", srv.URL, "--summary", "testdata/k6-summary.json", "--slos", "testdata/slos.yaml")
	if code != 1 || !strings.Contains(errOut, "report failed") || !strings.Contains(errOut, "step 1") {
		t.Errorf("exit %d, stderr %s", code, errOut)
	}
}

func TestResolvePlanDefaultsToNow(t *testing.T) {
	now := time.Date(2027, 1, 12, 11, 0, 0, 0, time.UTC)
	p, err := resolvePlan(reportOptions{from: "2027-01-12T10:00:00Z", steps: "5", stepDuration: 5 * time.Minute}, now)
	if err != nil || !p.End.Equal(now) || p.RunID != "" {
		t.Errorf("plan = %+v, %v", p, err)
	}
}

func TestCheckThresholds(t *testing.T) {
	slos := mustSLOs(t)
	if err := checkThresholds(slos, nil); err != nil {
		t.Errorf("no summary thresholds: %v", err)
	}
	if err := checkThresholds(slos, []float64{500, 1000}); err != nil {
		t.Errorf("all present: %v", err)
	}
	if err := checkThresholds(slos, []float64{500}); err == nil || !strings.Contains(err.Error(), "checkout") {
		t.Errorf("missing 1000: %v", err)
	}
}
