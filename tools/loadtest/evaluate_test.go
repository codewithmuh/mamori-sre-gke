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

	wantStatus := []string{statusInconclusive, statusPass, statusInconclusive, statusFail, statusInvalid}
	for i, s := range got {
		if s.Status != wantStatus[i] {
			t.Errorf("step %d status = %q, want %q (reasons %v)", s.Index, s.Status, wantStatus[i], s.Reasons)
		}
	}

	// Step 2: 4,800 requests in a 240 s window, every SLI clearly met.
	s2 := got[1]
	if s2.MeasuredRPS == nil || math.Abs(*s2.MeasuredRPS-20) > 1e-9 || math.Abs(*s2.Delivered-1) > 1e-9 {
		t.Errorf("step 2 measured = %v delivered = %v, want 20 and 1", deref(s2.MeasuredRPS), deref(s2.Delivered))
	}
	checkout := s2.Journeys[3]
	if checkout.ID != "checkout" || checkout.ThresholdMs != 1000 || checkout.Fast != 590 || !checkout.EnoughData || *checkout.P95Ms != 610 {
		t.Errorf("step 2 checkout = %+v", checkout)
	}
	if a := checkout.Availability; a.Verdict != sliMet || !near(a.Value, 599.0/600) || a.Target != 0.99 || !(a.Low < a.Value && a.Value < a.High) {
		t.Errorf("step 2 checkout availability = %+v", a)
	}
	if l := checkout.Latency; l.Verdict != sliMet || !near(l.Value, 590.0/600) || l.Target != 0.95 {
		t.Errorf("step 2 checkout latency = %+v", l)
	}
	if *s2.GeneratorCPU != 0.15 || *s2.NodeCPU != 0.62 || *s2.Nodes != 3 || *s2.FrontendRPS != 23.5 || *s2.Dropped != 0 || *s2.RateLimited != 0 {
		t.Errorf("step 2 context = %v %v %v %v %v %v", *s2.GeneratorCPU, *s2.NodeCPU, *s2.Nodes, *s2.FrontendRPS, *s2.Dropped, *s2.RateLimited)
	}
	if len(s2.Warnings) != 0 || len(s2.Reasons) != 0 {
		t.Errorf("step 2 warnings %v, reasons %v; want none", s2.Warnings, s2.Reasons)
	}

	// Step 1: no failures, but 300 view-cart requests cannot confirm 99.5 %. No dropped_iterations
	// series at all means 0, not unknown.
	if r := strings.Join(got[0].Reasons, "|"); r != "view-cart availability 100.00 %: 300 requests cannot tell it from the 99.5 % target (99.11 % to 100.00 %)" {
		t.Errorf("step 1 reasons = %q", r)
	}
	if v := got[0].Journeys[2].Availability.Verdict; v != sliUndecided {
		t.Errorf("step 1 view-cart availability verdict = %s", v)
	}
	if got[0].Dropped == nil || *got[0].Dropped != 0 {
		t.Errorf("step 1 dropped = %v, want 0", deref(got[0].Dropped))
	}

	// Step 3: view-cart at exactly 99.5 % on 1,200 requests is neither met nor missed.
	if r := strings.Join(got[2].Reasons, "|"); !strings.Contains(r, "view-cart availability 99.50 %: 1200 requests cannot tell") {
		t.Errorf("step 3 reasons = %q", r)
	}

	// Step 4 clearly misses checkout latency.
	if r := strings.Join(got[3].Reasons, "|"); r != "checkout requests under 1000 ms 83.33 % < 95.0 % (at most 84.73 %)" {
		t.Errorf("step 4 reasons = %q", r)
	}

	// Step 5: the 429s make it invalid even though its SLIs are missed; they come first.
	s5 := got[4]
	if !strings.HasPrefix(s5.Reasons[0], "30 requests got 429 Too Many Requests") {
		t.Errorf("step 5 reasons = %v", s5.Reasons)
	}
	r5 := strings.Join(s5.Reasons, "|")
	for _, want := range []string{"browse availability 95.00 % < 99.5 %", "k6 delivered 75 % of the planned rate"} {
		if !strings.Contains(r5, want) {
			t.Errorf("step 5 reasons %q lack %q", r5, want)
		}
	}
	w5 := strings.Join(s5.Warnings, "|")
	for _, want := range []string{"dropped 120 journeys", "k6 used 0.85 of its 1 CPU cores", "4 nodes reported"} {
		if !strings.Contains(w5, want) {
			t.Errorf("step 5 warnings %q lack %q", w5, want)
		}
	}

	// Every SLI query is limited to the run and to the 240 s measured window, at the step's end.
	for _, q := range fake.seen() {
		if strings.Contains(q, "k6_") && (!strings.Contains(q, `{run="20270112-100000"`) || !strings.Contains(q, "[240s]")) {
			t.Errorf("query %q is not limited to the run and window", q)
		}
	}

	c := findCapacity(got)
	if intOrZero(c.Max) != 2 || intOrZero(c.FirstNotPass) != 4 {
		t.Errorf("capacity = max %d, end %d; want 2 and 4", intOrZero(c.Max), intOrZero(c.FirstNotPass))
	}
	if w := strings.Join(c.Warnings, "|"); w != "step 3 was inconclusive: the limit lies between step 2 and step 4" {
		t.Errorf("capacity warnings = %q", w)
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
	if got[1].Status != statusPass || got[2].Status != statusNotRun || got[4].Status != statusNotRun {
		t.Errorf("statuses = %s %s %s", got[1].Status, got[2].Status, got[4].Status)
	}
	c := findCapacity(got)
	if intOrZero(c.Max) != 2 || intOrZero(c.FirstNotPass) != 3 {
		t.Errorf("capacity = %+v", c)
	}
}

func TestEvaluateWithoutData(t *testing.T) {
	fake := newFakeProm(t)
	fake.steps = make([]fixtureStep, len(fixtureSteps))
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	for _, s := range got {
		if s.Status != statusInvalid || !strings.Contains(s.Reasons[0], "no k6 data") {
			t.Errorf("step %d = %s %v, want invalid with no data", s.Index, s.Status, s.Reasons)
		}
	}
	if c := findCapacity(got); c.Max != nil || intOrZero(c.FirstNotPass) != 1 {
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

func TestEvaluateTooFewRequestsIsInconclusive(t *testing.T) {
	fake := newFakeProm(t)
	ev := newTestEvaluator(t, fake)
	ev.cfg.MinRequestsPerJourney = 700 // step 2 has 600 view-cart and 600 checkout requests
	got, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r := strings.Join(got[1].Reasons, "|")
	if got[1].Status != statusInconclusive || !strings.Contains(r, "view-cart: 600 requests, fewer than the 700 needed") || !strings.Contains(r, "checkout: 600 requests") {
		t.Errorf("step 2 = %s %q", got[1].Status, r)
	}
	// Inconclusive steps do not end the search; nothing passes before the failure at step 4.
	c := findCapacity(got)
	if c.Max != nil || intOrZero(c.FirstNotPass) != 4 || !strings.Contains(strings.Join(c.Warnings, "|"), "steps 1, 2 and 3 were inconclusive and step 4 did not pass") {
		t.Errorf("capacity = max %v, end %d, warnings %v", c.Max, intOrZero(c.FirstNotPass), c.Warnings)
	}
}

func TestEvaluateMissingJourney(t *testing.T) {
	fake := newFakeProm(t)
	for _, s := range fake.steps {
		delete(s.Requests, "view-cart")
		delete(s.Good, "view-cart")
	}
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	vc := got[1].Journeys[2]
	if vc.ID != "view-cart" || vc.Availability != nil || vc.Latency != nil || vc.EnoughData {
		t.Errorf("view-cart = %+v", vc)
	}
	// Without its 600 requests k6 delivered 87.5 % of the plan: the step is invalid.
	if got[1].Status != statusInvalid {
		t.Errorf("step 2 status = %s, reasons %v", got[1].Status, got[1].Reasons)
	}
}

func TestEvaluateCoreQueryErrorStops(t *testing.T) {
	for _, metric := range []string{"k6_mamori_cuj_requests_total", "k6_mamori_cuj_good_total", "k6_mamori_cuj_fast_total"} {
		fake := newFakeProm(t)
		fake.failOn[metric] = errors.New("permission denied")
		ev := newTestEvaluator(t, fake)
		_, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
		if err == nil || !strings.Contains(err.Error(), "step 1 (10 RPS)") || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: evaluate() error = %v", metric, err)
		}
	}
}

func TestEvaluateContextQueryErrorOnlyLosesThatNumber(t *testing.T) {
	fake := newFakeProm(t)
	fake.failOn["container_cpu_core_usage_time"] = errors.New("unknown metric")
	fake.failOn["histogram_quantile"] = errors.New("no histogram")
	fake.failOn["k6_http_reqs_total"] = errors.New("no such metric")
	ev := newTestEvaluator(t, fake)
	ev.cfg.Context.FrontendRPS = "" // skipped, never sent
	got, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s2 := got[1]
	if s2.Status != statusPass || s2.GeneratorCPU != nil || s2.FrontendRPS != nil || s2.RateLimited != nil || s2.Journeys[0].P95Ms != nil || s2.NodeCPU == nil {
		t.Errorf("step 2 = %+v", s2)
	}
	// Unknown 429s are not evidence of a limit: step 5 is judged on its SLIs.
	if got[4].Status != statusFail {
		t.Errorf("step 5 status = %s", got[4].Status)
	}
	for _, q := range fake.seen() {
		if strings.Contains(q, "istio_requests_total") {
			t.Errorf("empty context query was sent: %q", q)
		}
	}
}

func TestEvaluateWarnsAboutTooMuchLoad(t *testing.T) {
	fake := newFakeProm(t)
	s := fake.steps[1]
	for k := range s.Requests {
		s.Requests[k] *= 1.5
		s.Good[k] *= 1.5
		for th := range s.Fast[k] {
			s.Fast[k][th] *= 1.5
		}
	}
	got := evaluateFixture(t, fake, fixtureStart.Add(time.Hour))
	if !strings.Contains(strings.Join(got[1].Warnings, "|"), "measured 150 % of the planned rate") {
		t.Errorf("step 2 warnings = %v", got[1].Warnings)
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
	got, err := ev.evaluate(context.Background(), fixtureSchedule(t, time.Minute), fixtureStart.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got[4].Status != statusInvalid {
		t.Errorf("step 5 status = %s; the 429 query without a run label failed", got[4].Status)
	}
	for _, q := range fake.seen() {
		if strings.Contains(q, "{run=") {
			t.Errorf("query %q has a run selector", q)
		}
	}
}

func TestJudgeAndWilson(t *testing.T) {
	// No failures in 538 requests is not enough to confirm 99.5 % at z = 1.645; 539 is.
	if v := judge(538, 538, 0.995, 1.645).Verdict; v != sliUndecided {
		t.Errorf("538/538 = %s, want undecided", v)
	}
	if v := judge(539, 539, 0.995, 1.645).Verdict; v != sliMet {
		t.Errorf("539/539 = %s, want met", v)
	}
	if v := judge(90, 100, 0.995, 1.645).Verdict; v != sliMissed {
		t.Errorf("90/100 = %s, want missed", v)
	}
	if judge(1, 0, 0.995, 1.645) != nil {
		t.Error("judge with no requests is not nil")
	}
	// increase() can overshoot: the ratio is capped at 1, and the interval stays inside [0, 1].
	s := judge(101, 100, 0.5, 1.645)
	if s.Value != 1 || s.High != 1 || s.Low <= 0.9 {
		t.Errorf("judge(101, 100) = %+v", s)
	}
	if lo, hi := wilson(0, 10, 1.645); lo > 1e-9 || hi <= 0 || hi >= 1 {
		t.Errorf("wilson(0, 10) = %v, %v", lo, hi)
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
		end       int // 0: none
		warnHas   string
		warnCount int
	}{
		{"all pass", []string{"pass", "pass", "pass"}, 3, 0, "no step failed", 1},
		{"fails at 3", []string{"pass", "pass", "fail", "fail"}, 2, 3, "", 0},
		{"inconclusive is passed over", []string{"inconclusive", "pass", "inconclusive", "pass", "fail"}, 4, 5, "", 0},
		{"inconclusive before the end", []string{"pass", "inconclusive", "inconclusive", "fail"}, 1, 4, "steps 2 and 3 were inconclusive: the limit lies between step 1 and step 4", 1},
		{"invalid ends the search", []string{"pass", "invalid", "pass"}, 1, 2, "step 3 passed after step 2 did not", 1},
		{"not-run ends the search", []string{"pass", "not-run"}, 1, 2, "", 0},
		{"first step fails", []string{"fail", "pass"}, 0, 1, "step 2 passed after step 1", 1},
		{"only inconclusive", []string{"inconclusive", "inconclusive"}, 0, 0, "no step confirmed every SLO", 1},
		{"inconclusive then fail", []string{"inconclusive", "fail"}, 0, 2, "step 1 was inconclusive and step 2 did not pass", 1},
		{"nothing", nil, 0, 0, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := findCapacity(steps(tt.statuses...))
			if got := intOrZero(c.Max); got != tt.max {
				t.Errorf("max = %d, want %d", got, tt.max)
			}
			if got := intOrZero(c.FirstNotPass); got != tt.end {
				t.Errorf("end = %d, want %d", got, tt.end)
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
