package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func fixtureReport(t *testing.T, results []stepResult) report {
	t.Helper()
	cfg := mustConfig(t)
	plan := runPlan{RunID: "", Start: fixtureStart, End: fixtureStart.Add(time.Hour), Steps: fixtureSteps, StepDuration: fixtureStepDuration}
	return buildReport(cfg, mustSLOs(t), plan, results, "a test", fixtureStart.Add(2*time.Hour))
}

func TestMarkdownWhenNothingPassed(t *testing.T) {
	results := []stepResult{
		{Index: 1, PlannedRPS: 5, Status: statusFail, Reasons: []string{"browse availability 90.000 % < 99.5 %"}},
		{Index: 2, PlannedRPS: 10, Status: statusNotRun, Reasons: []string{"the step had not finished by the end of the time range"}},
	}
	var b bytes.Buffer
	if err := writeMarkdown(&b, fixtureReport(t, results)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"run `(all runs in the time range)`",
		"none: the first step did not pass, so start lower",
		"| Cost per 1K RPS | not computed: no step held every SLO |",
		"| 1 | 5 | n/a | n/a | — | — | — | — | n/a | n/a | n/a | n/a | fail |",
		"- Step 1 (5 RPS), fail: browse availability",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "Step 2 (10 RPS), not-run") {
		t.Error("a not-run step is listed as a failure")
	}
}

func TestMarkdownAllPassedAndNoRequests(t *testing.T) {
	one := 1.0
	results := []stepResult{{
		Index:       1,
		PlannedRPS:  5,
		Status:      statusPass,
		MeasuredRPS: &one,
		Delivered:   &one,
		Journeys:    []journeyResult{{ID: "browse"}},
	}}
	r := fixtureReport(t, results)
	var b bytes.Buffer
	if err := writeMarkdown(&b, r); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"| Search ended at | no step failed |", "no requests", "no step failed: the limit is above", "**1.0 req/s**"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "without the load generator") {
		t.Error("net cost shown without generator CPU")
	}
}

func TestMarkdownWhenNoStepConfirmed(t *testing.T) {
	results := []stepResult{
		{Index: 1, PlannedRPS: 10, Status: statusInconclusive, Reasons: []string{"view-cart: 50 requests, fewer than the 100 needed"}},
		{Index: 2, PlannedRPS: 20, Status: statusFail, Reasons: []string{"checkout availability 90.00 % < 99.0 %"}},
	}
	var b bytes.Buffer
	if err := writeMarkdown(&b, fixtureReport(t, results)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"| Highest rate that held every SLO | none: no step confirmed every SLO (see Warnings) |",
		"| Search ended at | step 2 (20 RPS), fail: checkout availability",
		"- Step 1 (10 RPS), inconclusive: view-cart: 50 requests",
		"- step 1 was inconclusive and step 2 did not pass",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q\n%s", want, out)
		}
	}
}

func TestWriteMarkdownError(t *testing.T) {
	if err := writeMarkdown(failingWriter{}, fixtureReport(t, nil)); err == nil {
		t.Error("want the writer's error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestMoney(t *testing.T) {
	for in, want := range map[float64]string{0.16116: "$0.1612", 4.054: "$4.05", 117.65: "$118", 2957.3: "$2,957", 1234567: "$1,234,567", -2500: "$-2,500"} {
		if got := money(in, "USD"); got != want {
			t.Errorf("money(%v) = %q, want %q", in, got, want)
		}
	}
	if got := money(12.3, "EUR"); got != "12.30 EUR" {
		t.Errorf("money EUR = %q", got)
	}
}
