package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Step verdicts. Only fail, invalid and not-run end the search for the capacity.
const (
	statusPass         = "pass"         // every SLI clearly met, at the planned rate
	statusFail         = "fail"         // at least one SLI clearly missed
	statusInconclusive = "inconclusive" // nothing clearly missed, but too few requests to confirm every SLI
	statusInvalid      = "invalid"      // not a test of the shop at the planned rate: 429s, k6 fell behind, or no data
	statusNotRun       = "not-run"      // the step had not finished by the end of the time range
)

// SLI verdicts, from the Wilson score interval of the measured ratio.
const (
	sliMet       = "met"       // the interval's lower end is at or above the target
	sliMissed    = "missed"    // the interval's upper end is below the target
	sliUndecided = "undecided" // the target lies inside the interval: too few requests to tell
)

// sli is one measured ratio with its interval and verdict.
type sli struct {
	Value   float64 `json:"value"`
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
	Target  float64 `json:"target"`
	Verdict string  `json:"verdict"`
}

type journeyResult struct {
	ID           string   `json:"id"`
	Requests     float64  `json:"requests"`
	Good         float64  `json:"good"`
	Fast         float64  `json:"fast"`
	Availability *sli     `json:"availability"`
	Latency      *sli     `json:"latency"`
	ThresholdMs  float64  `json:"thresholdMs"`
	P95Ms        *float64 `json:"p95Ms,omitempty"`
	EnoughData   bool     `json:"enoughData"`
}

type stepResult struct {
	Index        int             `json:"index"`
	PlannedRPS   float64         `json:"plannedRps"`
	Start        time.Time       `json:"start"`
	MeasuredFrom time.Time       `json:"measuredFrom"`
	End          time.Time       `json:"end"`
	Status       string          `json:"status"`
	MeasuredRPS  *float64        `json:"measuredRps,omitempty"`
	Delivered    *float64        `json:"delivered,omitempty"`
	Dropped      *float64        `json:"droppedIterations,omitempty"`
	RateLimited  *float64        `json:"rateLimitedRequests,omitempty"`
	GeneratorCPU *float64        `json:"generatorCpuCores,omitempty"`
	NodeCPU      *float64        `json:"nodeCpuUtilization,omitempty"`
	Nodes        *float64        `json:"nodesSeen,omitempty"`
	FrontendRPS  *float64        `json:"frontendRps,omitempty"`
	Journeys     []journeyResult `json:"journeys,omitempty"`
	Reasons      []string        `json:"reasons,omitempty"`
	Warnings     []string        `json:"warnings,omitempty"`
}

// evaluator reads one run's SLIs per step and decides each step.
type evaluator struct {
	q     querier
	cfg   config
	slos  sloSpec
	runID string
	log   *slog.Logger
}

// evaluate decides every step that finished before `to`; later steps are not-run.
func (e *evaluator) evaluate(ctx context.Context, steps []step, to time.Time) ([]stepResult, error) {
	out := make([]stepResult, 0, len(steps))
	for _, s := range steps {
		r := stepResult{Index: s.Index, PlannedRPS: s.PlannedRPS, Start: s.Start, MeasuredFrom: s.MeasuredFrom, End: s.End}
		if s.End.After(to) {
			r.Status = statusNotRun
			r.Reasons = []string{"the step had not finished by the end of the time range"}
			out = append(out, r)
			continue
		}
		if err := e.evaluateStep(ctx, s, &r); err != nil {
			return nil, fmt.Errorf("step %d (%g RPS): %w", s.Index, s.PlannedRPS, err)
		}
		e.log.InfoContext(ctx, "step evaluated", "step", s.Index, "planned_rps", s.PlannedRPS, "status", r.Status,
			"reasons", r.Reasons)
		out = append(out, r)
	}
	return out, nil
}

// selector limits a k6 metric to the run, plus any extra label matchers.
func (e *evaluator) selector(matchers ...string) string {
	if e.runID != "" {
		matchers = append([]string{fmt.Sprintf("%s=%q", e.cfg.Metrics.RunLabel, e.runID)}, matchers...)
	}
	if len(matchers) == 0 {
		return ""
	}
	return "{" + strings.Join(matchers, ",") + "}"
}

// counterIncrease sums a counter's increase over the window, grouped by the given labels.
func (e *evaluator) counterIncrease(metric, window string, by ...string) string {
	return fmt.Sprintf("sum by (%s) (increase(%s%s[%s]))", strings.Join(by, ", "), metric, e.selector(), window)
}

func (e *evaluator) evaluateStep(ctx context.Context, s step, r *stepResult) error {
	m := e.cfg.Metrics
	window := promDuration(s.window())
	at := s.End

	requests, err := e.byLabel(ctx, e.counterIncrease(m.Requests, window, m.JourneyLabel), at, m.JourneyLabel)
	if err != nil {
		return err
	}
	good, err := e.byLabel(ctx, e.counterIncrease(m.Good, window, m.JourneyLabel), at, m.JourneyLabel)
	if err != nil {
		return err
	}
	fastSamples, err := e.q.Query(ctx, e.counterIncrease(m.Fast, window, m.JourneyLabel, m.ThresholdLabel), at)
	if err != nil {
		return fmt.Errorf("query %s: %w", m.Fast, err)
	}
	fast := map[string]map[string]float64{} // journey -> threshold label -> count
	thresholds := map[string]bool{}
	for _, smp := range fastSamples {
		j, t := smp.Labels[m.JourneyLabel], smp.Labels[m.ThresholdLabel]
		if fast[j] == nil {
			fast[j] = map[string]float64{}
		}
		fast[j][t] += finite(smp.Value)
		thresholds[t] = true
	}

	var total float64
	for _, v := range requests {
		total += v
	}
	if total == 0 {
		r.Status = statusInvalid
		r.Reasons = []string{"no k6 data in this window (check the run id, the time range and that the relay was scraped)"}
		return nil
	}

	seconds := s.window().Seconds()
	measured := total / seconds
	delivered := measured / s.PlannedRPS
	r.MeasuredRPS, r.Delivered = &measured, &delivered

	// The ingress gateway's local rate limit (deploy/resilience/ingress-rate-limit.yaml) answers 429.
	// `make loadtest-run` turns its enforcement off; 429s mean it was on, or a gateway pod restarted.
	tooMany := e.selector(fmt.Sprintf("%s=%q", m.StatusLabel, "429"))
	r.RateLimited = e.optionalScalar(ctx, "429 responses", fmt.Sprintf("sum(increase(%s%s[%s]))", m.HTTPRequests, tooMany, window), at, true)

	var limited, misses, load, undecided, gaps []string
	if r.RateLimited != nil && *r.RateLimited >= 1 {
		limited = append(limited, fmt.Sprintf("%.0f requests got 429 Too Many Requests: the ingress gateway's rate limit was enforced, so this step measured the limit, not the shop", *r.RateLimited))
	}
	z := e.cfg.ConfidenceZ
	for _, slo := range e.slos.Journeys {
		label := thresholdLabel(slo.Latency.ThresholdMs)
		if good[slo.ID] > 0 && !thresholds[label] {
			return fmt.Errorf("the run did not count requests under %s ms (it counted under: %s ms); add %s to LATENCY_THRESHOLDS_MS and run again",
				label, strings.Join(sortedKeys(thresholds), ", "), label)
		}
		j := journeyResult{
			ID:          slo.ID,
			Requests:    requests[slo.ID],
			Good:        good[slo.ID],
			Fast:        fast[slo.ID][label],
			ThresholdMs: slo.Latency.ThresholdMs,
			EnoughData:  requests[slo.ID] >= e.cfg.MinRequestsPerJourney,
		}
		// A failed request never counts as fast, so both ratios share the denominator.
		j.Availability = judge(j.Good, j.Requests, slo.Availability.Target, z)
		j.Latency = judge(j.Fast, j.Requests, slo.Latency.Target, z)
		for _, c := range []struct {
			name string
			s    *sli
		}{
			{slo.ID + " availability", j.Availability},
			{fmt.Sprintf("%s requests under %s ms", slo.ID, label), j.Latency},
		} {
			switch {
			case c.s == nil:
			case c.s.Verdict == sliMissed:
				misses = append(misses, fmt.Sprintf("%s %s < %s (at most %s)", c.name, pct(c.s.Value, 2), pct(c.s.Target, 1), pct(c.s.High, 2)))
			case c.s.Verdict == sliUndecided:
				undecided = append(undecided, fmt.Sprintf("%s %s: %.0f requests cannot tell it from the %s target (%s to %s)",
					c.name, pct(c.s.Value, 2), j.Requests, pct(c.s.Target, 1), pct(c.s.Low, 2), pct(c.s.High, 2)))
			}
		}
		if !j.EnoughData {
			gaps = append(gaps, fmt.Sprintf("%s: %.0f requests, fewer than the %.0f needed", j.ID, j.Requests, e.cfg.MinRequestsPerJourney))
		}
		r.Journeys = append(r.Journeys, j)
	}
	if delivered < e.cfg.MinDeliveredRatio {
		load = append(load, fmt.Sprintf("k6 delivered %s of the planned rate (VUs or CPU ran out)", pct(delivered, 0)))
	}
	if delivered > 1.2 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("measured %s of the planned rate: another run in the window, or the wrong --steps or --run-id?", pct(delivered, 0)))
	}

	// 429s come first: they are failures the shop did not cause, so a miss beside them proves nothing.
	switch {
	case len(limited) > 0:
		r.Status = statusInvalid
	case len(misses) > 0:
		r.Status = statusFail
	case len(load) > 0:
		r.Status = statusInvalid
	case len(undecided) > 0 || len(gaps) > 0:
		r.Status = statusInconclusive
	default:
		r.Status = statusPass
	}
	for _, part := range [][]string{limited, misses, load, undecided, gaps} {
		r.Reasons = append(r.Reasons, part...)
	}

	e.addContext(ctx, s, r, window)
	return nil
}

// judge rates k good out of n against a target. Nil when there were no requests.
func judge(k, n, target, z float64) *sli {
	if !(n > 0) {
		return nil
	}
	// increase() extrapolates to the window's edges, so a ratio can come out a hair over 1.
	v := math.Max(0, math.Min(k/n, 1))
	lo, hi := wilson(v, n, z)
	s := &sli{Value: v, Low: lo, High: hi, Target: target, Verdict: sliUndecided}
	switch {
	case lo >= target:
		s.Verdict = sliMet
	case hi < target:
		s.Verdict = sliMissed
	}
	return s
}

// wilson is the Wilson score interval of a ratio p measured on n requests. Unlike p ± z·σ it stays
// inside [0, 1] and is honest near 100 %: with no failures in 200 requests and z = 1.645 its lower
// end is 98.7 %, so 200 requests cannot show a 99.5 % target is met.
func wilson(p, n, z float64) (lo, hi float64) {
	z2 := z * z
	denom := 1 + z2/n
	center := (p + z2/(2*n)) / denom
	half := z / denom * math.Sqrt(p*(1-p)/n+z2/(4*n*n))
	return math.Max(0, center-half), math.Min(1, center+half)
}

// addContext fills the informational numbers. A failed query only costs that number.
func (e *evaluator) addContext(ctx context.Context, s step, r *stepResult, window string) {
	m := e.cfg.Metrics
	at := s.End

	r.Dropped = e.optionalScalar(ctx, "dropped iterations", fmt.Sprintf("sum(increase(%s%s[%s]))", m.Dropped, e.selector(), window), at, true)
	if r.Dropped != nil && *r.Dropped >= 1 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("k6 dropped %.0f journeys: it had no free VU to start them (raise MAX_VUS, or the shop is very slow)", *r.Dropped))
	}

	p95q := fmt.Sprintf("histogram_quantile(0.95, sum by (%s, le) (rate(%s_bucket%s[%s])))", m.JourneyLabel, m.Duration, e.selector(), window)
	if p95, err := e.byLabel(ctx, p95q, at, m.JourneyLabel); err != nil {
		e.log.WarnContext(ctx, "p95 latency not available", "step", s.Index, "error", err.Error())
	} else {
		for i := range r.Journeys {
			if v, ok := p95[r.Journeys[i].ID]; ok {
				r.Journeys[i].P95Ms = &v
			}
		}
	}

	expand := strings.NewReplacer("${range}", window, "${cluster}", e.cfg.Cluster).Replace
	c := e.cfg.Context
	r.GeneratorCPU = e.optionalScalar(ctx, "generator CPU", expand(c.GeneratorCPU), at, false)
	r.NodeCPU = e.optionalScalar(ctx, "node CPU", expand(c.NodeCPU), at, false)
	r.Nodes = e.optionalScalar(ctx, "nodes", expand(c.Nodes), at, false)
	r.FrontendRPS = e.optionalScalar(ctx, "frontend RPS", expand(c.FrontendRPS), at, false)

	if r.GeneratorCPU != nil && *r.GeneratorCPU > 0.8*e.cfg.GeneratorCPULimit {
		r.Warnings = append(r.Warnings, fmt.Sprintf("k6 used %.2f of its %g CPU cores: the generator itself may have added latency", *r.GeneratorCPU, e.cfg.GeneratorCPULimit))
	}
	if r.Nodes != nil && int(math.Round(*r.Nodes)) != e.cfg.Cost.Nodes {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%.0f nodes reported in this window, %d expected: a preemption, repair or surge upgrade changed the capacity", *r.Nodes, e.cfg.Cost.Nodes))
	}
}

// optionalScalar runs a query that should return one number. Empty queries are skipped. With
// zeroIfEmpty, no series means 0 (a counter that never moved).
func (e *evaluator) optionalScalar(ctx context.Context, name, query string, at time.Time, zeroIfEmpty bool) *float64 {
	if query == "" {
		return nil
	}
	samples, err := e.q.Query(ctx, query, at)
	if err != nil {
		e.log.WarnContext(ctx, "context query failed", "what", name, "error", err.Error())
		return nil
	}
	switch {
	case len(samples) == 0 && zeroIfEmpty:
		v := 0.0
		return &v
	case len(samples) != 1 || math.IsNaN(samples[0].Value) || math.IsInf(samples[0].Value, 0):
		e.log.WarnContext(ctx, "context query returned no single number", "what", name, "series", len(samples))
		return nil
	}
	v := samples[0].Value
	return &v
}

// byLabel runs a query and keys its finite results by one label.
func (e *evaluator) byLabel(ctx context.Context, query string, at time.Time, label string) (map[string]float64, error) {
	samples, err := e.q.Query(ctx, query, at)
	if err != nil {
		return nil, fmt.Errorf("query %q: %w", query, err)
	}
	out := map[string]float64{}
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		out[s.Labels[label]] += s.Value
	}
	return out, nil
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// sortedKeys sorts threshold labels as numbers where they are numbers.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, errA := strconv.ParseFloat(keys[i], 64)
		b, errB := strconv.ParseFloat(keys[j], 64)
		if errA == nil && errB == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	if len(keys) == 0 {
		return []string{"none"}
	}
	return keys
}

// capacity is the highest passing step before the first step that failed, was invalid or did not
// run. Inconclusive steps (too few requests to judge) are passed over, not counted: they neither
// confirm nor refute the SLOs. A pass after the end of the search does not count either: it would
// be a noisy run, not more capacity.
type capacity struct {
	Max          *int // 1-based index of the highest passing step
	FirstNotPass *int // the step that ended the search
	Warnings     []string
}

func findCapacity(results []stepResult) capacity {
	var c capacity
	var skipped []int // inconclusive steps since the last pass
	for i := range results {
		switch results[i].Status {
		case statusPass:
			idx := results[i].Index
			c.Max, skipped = &idx, nil
			continue
		case statusInconclusive:
			skipped = append(skipped, results[i].Index)
			continue
		}
		end := results[i].Index
		c.FirstNotPass = &end
		switch {
		case c.Max != nil && len(skipped) > 0:
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s inconclusive: the limit lies between step %d and step %d", stepsWere(skipped), *c.Max, end))
		case c.Max == nil && len(skipped) > 0:
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s inconclusive and step %d did not pass: no step confirmed every SLO; lengthen STEP_DURATION for more requests per step", stepsWere(skipped), end))
		}
		for _, later := range results[i+1:] {
			if later.Status == statusPass {
				c.Warnings = append(c.Warnings, fmt.Sprintf("step %d passed after step %d did not: the run is noisy; repeat it before trusting the result", later.Index, end))
				break
			}
		}
		return c
	}
	switch {
	case c.Max != nil:
		c.Warnings = append(c.Warnings, "no step failed: the limit is above the highest step that passed; run again with higher STEPS")
	case len(skipped) > 0:
		c.Warnings = append(c.Warnings, "no step confirmed every SLO: lengthen STEP_DURATION or raise STEPS for more requests per step")
	}
	return c
}

// stepsWere writes "step 3 was" or "steps 3 and 4 were".
func stepsWere(v []int) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.Itoa(x)
	}
	if len(parts) == 1 {
		return "step " + parts[0] + " was"
	}
	return "steps " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1] + " were"
}

// pct writes a ratio as a percentage, e.g. pct(0.995, 1) = "99.5 %".
func pct(v float64, decimals int) string {
	return strconv.FormatFloat(v*100, 'f', decimals, 64) + " %"
}
