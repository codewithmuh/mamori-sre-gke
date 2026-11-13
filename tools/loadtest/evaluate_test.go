package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func newTestEvaluator(t *testing.T, q querier) *evaluator {
	t.Helper()
	return &evaluator{q: q, cfg: mustConfig(t), slos: mustSLOs(t), runID: fixtureRunID, log: discardLogger()}
}

func evaluateFixture(t *testing.T, fake *fakeProm, to time.Time) []stepResult {
	t.Helper()
	ev := newTestEvaluator(t, fake)
	got, err := ev.evaluate(context.Background(), fixtureSchedule(t, ev.cfg.Warmup.Duration), to)
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	return got
}

func TestEvaluateFixtureRun(t *testing.T) {
	fake := newFakeProm(t)
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))

	wantStatus := []string{statusPass, statusPass, statusFail, statusFail}
	for i, s := range got {
		if s.Status != wantStatus[i] {
			t.Errorf("step %d status = %q, want %q (reasons %v)", s.Index, s.Status, wantStatus[i], s.Reasons)
		}
	}

	// Step 2: 2,400 requests in a 240 s window.
	s2 := got[1]
	if s2.MeasuredRPS == nil || math.Abs(*s2.MeasuredRPS-10) > 1e-9 || math.Abs(*s2.Delivered-1) > 1e-9 {
		t.Errorf("step 2 measured = %v delivered = %v, want 10 and 1", deref(s2.MeasuredRPS), deref(s2.Delivered))
	}
	checkout := s2.Journeys[3]
	if checkout.ID != "checkout" || checkout.ThresholdMs != 1000 || checkout.Fast != 197 || !checkout.EnoughData {
		t.Errorf("step 2 checkout = %+v", checkout)
	}
	if math.Abs(*checkout.Availability-0.995) > 1e-9 || math.Abs(*checkout.LatencyShare-0.985) > 1e-9 || *checkout.P95Ms != 560 {
		t.Errorf("step 2 checkout SLIs = %v / %v / %v", *checkout.Availability, *checkout.LatencyShare, *checkout.P95Ms)
	}
	if *s2.GeneratorCPU != 0.09 || *s2.NodeCPU != 0.55 || *s2.Nodes != 3 || *s2.FrontendRPS != 12 || *s2.Dropped != 0 || *s2.RateLimited != 0 {
		t.Errorf("step 2 context = %v %v %v %v %v %v", *s2.GeneratorCPU, *s2.NodeCPU, *s2.Nodes, *s2.FrontendRPS, *s2.Dropped, *s2.RateLimited)
	}
	if len(s2.Warnings) != 0 {
		t.Errorf("step 2 warnings = %v, want none", s2.Warnings)
	}

	// Step 1 has no dropped_iterations series at all: that means 0, not unknown.
	if got[0].Dropped == nil || *got[0].Dropped != 0 {
		t.Errorf("step 1 dropped = %v, want 0", deref(got[0].Dropped))
	}

	// Step 3 holds availability (checkout exactly at 99.0 %) but misses checkout latency.
	if r := strings.Join(got[2].Reasons, "|"); r != "checkout: 90.0 % of requests under 1000 ms < 95.0 %" {
		t.Errorf("step 3 reasons = %q", r)
	}

	// Step 4 misses availability, k6 fell behind, and the context raises three warnings.
	s4 := strings.Join(got[3].Reasons, "|")
	for _, want := range []string{"browse availability 95.000 % < 99.5 %", "k6 delivered 75 % of the planned rate"} {
		if !strings.Contains(s4, want) {
			t.Errorf("step 4 reasons %q lack %q", s4, want)
		}
	}
	w4 := strings.Join(got[3].Warnings, "|")
	for _, want := range []string{"dropped 120 journeys", "k6 used 0.85 of its 1 CPU cores", "4 nodes reported", "30 requests got 429"} {
		if !strings.Contains(w4, want) {
			t.Errorf("step 4 warnings %q lack %q", w4, want)
		}
	}

	// Every SLI query is limited to the run and to the 240 s measured window, at the step's end.
	for _, q := range fake.seen() {
		if strings.Contains(q, "k6_") && (!strings.Contains(q, `{run="20270112-100000"`) || !strings.Contains(q, "[240s]")) {
			t.Errorf("query %q is not limited to the run and window", q)
		}
	}
}

func TestEvaluateQueriesAtStepEnd(t *testing.T) {
	fake := newFakeProm(t)
	evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	ends := map[time.Time]bool{}
	for i := 1; i <= len(fixtureSteps); i++ {
		ends[fixtureStart.Add(time.Duration(i)*fixtureStepDuration)] = true
	}
	for _, at := range fake.queryAts {
		if !ends[at] {
			t.Errorf("query at %s is not at a step's end", at)
		}
	}
}

func TestEvaluateStepsAfterTheRangeAreNotRun(t *testing.T) {
	fake := newFakeProm(t)
	// The range ends in the middle of step 3.
	got := evaluateFixture(t, fake, fixtureStart.Add(2*fixtureStepDuration+time.Minute))
	if got[1].Status != statusPass || got[2].Status != statusNotRun || got[3].Status != statusNotRun {
		t.Errorf("statuses = %s %s %s", got[1].Status, got[2].Status, got[3].Status)
	}
	c := findCapacity(got)
	if c.Max == nil || *c.Max != 2 || *c.FirstNotPass != 3 {
		t.Errorf("capacity = %+v", c)
	}
}

func TestEvaluateWithoutData(t *testing.T) {
	fake := newFakeProm(t)
	fake.steps = []fixtureStep{{}, {}, {}, {}}
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	for _, s := range got {
		if s.Status != statusInvalid || !strings.Contains(s.Reasons[0], "no k6 data") {
			t.Errorf("step %d = %s %v, want invalid with no data", s.Index, s.Status, s.Reasons)
		}
	}
	if c := findCapacity(got); c.Max != nil || *c.FirstNotPass != 1 {
		t.Errorf("capacity = %+v, want no max", c)
	}
}

func TestEvaluateNeedsTheSLOThreshold(t *testing.T) {
	fake := newFakeProm(t)
	for _, s := range fake.steps {
		for _, cuj := range sortedMapKeys(s.Fast) {
			delete(s.Fast[cuj], "1000")
		}
	}
	ev := newTestEvaluator(t, fake)
	_, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "did not count requests under 1000 ms (it counted under: 100, 250, 500 ms)") {
		t.Fatalf("evaluate() error = %v", err)
	}
}

func TestEvaluateTooFewRequestsIsInvalid(t *testing.T) {
	fake := newFakeProm(t)
	ev := newTestEvaluator(t, fake)
	ev.cfg.MinRequestsPerJourney = 150 // step 1 has 100 view-cart and 100 checkout requests
	got, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Status != statusInvalid || len(got[0].Reasons) != 2 || !strings.Contains(got[0].Reasons[0], "view-cart: 100 requests, fewer than the 150 needed") {
		t.Errorf("step 1 = %s %v", got[0].Status, got[0].Reasons)
	}
}

func TestEvaluateMissingJourney(t *testing.T) {
	fake := newFakeProm(t)
	for _, s := range fake.steps {
		delete(s.Requests, "view-cart")
		delete(s.Good, "view-cart")
	}
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	vc := got[0].Journeys[2]
	if vc.ID != "view-cart" || vc.Availability != nil || vc.EnoughData || got[0].Status != statusInvalid {
		t.Errorf("view-cart = %+v, step 1 status %s", vc, got[0].Status)
	}
}

func TestEvaluateCoreQueryErrorStops(t *testing.T) {
	for _, metric := range []string{"k6_mamori_cuj_requests_total", "k6_mamori_cuj_good_total", "k6_mamori_cuj_fast_total"} {
		fake := newFakeProm(t)
		fake.failOn[metric] = errors.New("permission denied")
		ev := newTestEvaluator(t, fake)
		_, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
		if err == nil || !strings.Contains(err.Error(), "step 1 (5 RPS)") || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: evaluate() error = %v", metric, err)
		}
	}
}

func TestEvaluateContextQueryErrorOnlyLosesThatNumber(t *testing.T) {
	fake := newFakeProm(t)
	fake.failOn["container_cpu_core_usage_time"] = errors.New("unknown metric")
	fake.failOn["histogram_quantile"] = errors.New("no histogram")
	ev := newTestEvaluator(t, fake)
	ev.cfg.Context.FrontendRPS = "" // skipped, never sent
	got, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Status != statusPass || got[1].GeneratorCPU != nil || got[1].FrontendRPS != nil || got[1].Journeys[0].P95Ms != nil || got[1].NodeCPU == nil {
		t.Errorf("step 2 = %+v", got[1])
	}
	for _, q := range fake.seen() {
		if strings.Contains(q, "istio_requests_total") {
			t.Errorf("empty context query was sent: %q", q)
		}
	}
}

func TestEvaluateWarnsAboutTooMuchLoad(t *testing.T) {
	fake := newFakeProm(t)
	s := fake.steps[0]
	for k := range s.Requests {
		s.Requests[k] *= 1.5
		s.Good[k] *= 1.5
		for th := range s.Fast[k] {
			s.Fast[k][th] *= 1.5
		}
	}
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	if !strings.Contains(strings.Join(got[0].Warnings, "|"), "measured 150 % of the planned rate") {
		t.Errorf("step 1 warnings = %v", got[0].Warnings)
	}
}

func TestEvaluateIgnoresNaNAndSingleNumberShape(t *testing.T) {
	fake := newFakeProm(t)
	nan := math.NaN()
	fake.steps[0].NodeCPU = &nan
	fake.steps[0].P95["browse"] = math.Inf(1)
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	if got[0].NodeCPU != nil || got[0].Journeys[0].P95Ms != nil {
		t.Errorf("NaN/Inf leaked into step 1: node %v p95 %v", deref(got[0].NodeCPU), deref(got[0].Journeys[0].P95Ms))
	}
}

func TestEvaluateWithoutRunID(t *testing.T) {
	fake := newFakeProm(t)
	ev := newTestEvaluator(t, fake)
	ev.runID = ""
	if _, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, q := range fake.seen() {
		if strings.Contains(q, "{run=") {
			t.Errorf("query %q has a run selector", q)
		}
	}
}

func TestFindCapacity(t *testing.T) {
	steps := func(statuses ...string) []stepResult {
		out := make([]stepResult, len(statuses))
		for i, s := range statuses {
			out[i] = stepResult{Index: i + 1, Status: s}
		}
		return out
	}
	tests := []struct {
		name      string
		statuses  []string
		max       int // 0: none
		firstNot  int // 0: none
		warnHas   string
		warnCount int
	}{
		{"all pass", []string{"pass", "pass", "pass"}, 3, 0, "limit is above the last step", 1},
		{"fails at 3", []string{"pass", "pass", "fail", "fail"}, 2, 3, "", 0},
		{"invalid stops the search", []string{"pass", "invalid", "pass"}, 1, 2, "step 3 passed after step 2 did not", 1},
		{"first step fails", []string{"fail", "pass"}, 0, 1, "step 2 passed after step 1", 1},
		{"nothing", nil, 0, 0, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := findCapacity(steps(tt.statuses...))
			if got := intOrZero(c.Max); got != tt.max {
				t.Errorf("max = %d, want %d", got, tt.max)
			}
			if got := intOrZero(c.FirstNotPass); got != tt.firstNot {
				t.Errorf("first not passing = %d, want %d", got, tt.firstNot)
			}
			if len(c.Warnings) != tt.warnCount || (tt.warnHas != "" && !strings.Contains(c.Warnings[0], tt.warnHas)) {
				t.Errorf("warnings = %v", c.Warnings)
			}
		})
	}
}

func TestSortedKeysAndPct(t *testing.T) {
	got := strings.Join(sortedKeys(map[string]bool{"1000": true, "250": true, "100": true, "1500": true}), ",")
	if got != "100,250,1000,1500" {
		t.Errorf("sortedKeys = %s", got)
	}
	if got := sortedKeys(nil); len(got) != 1 || got[0] != "none" {
		t.Errorf("sortedKeys(nil) = %v", got)
	}
	if got := pct(0.995, 1); got != "99.5 %" {
		t.Errorf("pct = %q", got)
	}
	if finite(math.NaN()) != 0 || finite(2) != 2 {
		t.Error("finite()")
	}
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func intOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
