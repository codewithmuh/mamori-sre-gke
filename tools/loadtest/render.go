package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// report is everything `loadtest report` found, in the shape of the JSON output.
type report struct {
	GeneratedAt         time.Time    `json:"generatedAt"`
	Source              string       `json:"source"`
	RunID               string       `json:"runId"`
	From                time.Time    `json:"from"`
	To                  time.Time    `json:"to"`
	StepDuration        duration     `json:"stepDuration"`
	Warmup              duration     `json:"warmup"`
	Cluster             string       `json:"cluster"`
	MinDeliveredRatio   float64      `json:"minDeliveredRatio"`
	MinRequests         float64      `json:"minRequestsPerJourney"`
	ConfidenceZ         float64      `json:"confidenceZ"`
	CostModel           costModel    `json:"costModel"`
	SLOs                []journeySLO `json:"slos"`
	Steps               []stepResult `json:"steps"`
	MaxStep             *int         `json:"maxStep"`
	FirstNotPassingStep *int         `json:"firstNotPassingStep"`
	MaxRPS              *float64     `json:"maxRps"`
	Cost                *costResult  `json:"cost"`
	// RateLimitShare × MaxRPS, rounded down: a value for the ingress gateway's local rate limit.
	SuggestedRateLimit *float64 `json:"suggestedRateLimitRps"`
	RateLimitShare     float64  `json:"rateLimitShare"`
	Warnings           []string `json:"warnings"`
}

func writeJSON(w io.Writer, r report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

const timeLayout = "2006-01-02 15:04:05 UTC"

func writeMarkdown(w io.Writer, r report) error {
	var b bytes.Buffer
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	m := r.CostModel

	run := r.RunID
	if run == "" {
		run = "(all runs in the time range)"
	}
	p("# Capacity report: run `%s`", run)
	p("")
	p("Lab measurement with synthetic load on a small Spot cluster, not a production figure. Made by")
	p("`tools/loadtest` on %s from %s.", r.GeneratedAt.UTC().Format(timeLayout), r.Source)
	p("")
	p("| | |")
	p("|---|---|")
	p("| Run | `%s`, %s to %s |", run, r.From.UTC().Format(timeLayout), r.To.UTC().Format(timeLayout))
	p("| Cluster | `%s`: %d × %s %s, %s |", r.Cluster, m.Nodes, m.MachineType, m.Provisioning, m.Region)
	p("| Steps | %s RPS; %s each; the first %s of each step not measured |", joinNumbers(stepRates(r.Steps)), r.StepDuration, r.Warmup)
	p("| Pass rule | for every journey, availability and latency above target at the lower end of the Wilson interval (z = %g), on at least %.0f requests; k6 delivers at least %s of the planned rate; no 429s. Inconclusive steps are passed over |",
		r.ConfidenceZ, r.MinRequests, pct(r.MinDeliveredRatio, 0))
	p("")

	p("## Result")
	p("")
	p("| Measure | Value |")
	p("|---|---|")
	switch {
	case r.MaxStep != nil && r.MaxRPS != nil:
		s := r.Steps[*r.MaxStep-1]
		p("| Highest rate that held every SLO | **%s req/s** (step %d, planned %s RPS) |", num(*r.MaxRPS, 1), s.Index, num(s.PlannedRPS, -1))
	case r.FirstNotPassingStep != nil && *r.FirstNotPassingStep == 1:
		p("| Highest rate that held every SLO | none: the first step did not pass, so start lower |")
	default:
		p("| Highest rate that held every SLO | none: no step confirmed every SLO (see Warnings) |")
	}
	if r.FirstNotPassingStep != nil {
		s := r.Steps[*r.FirstNotPassingStep-1]
		p("| Search ended at | step %d (%s RPS), %s: %s |", s.Index, num(s.PlannedRPS, -1), s.Status, strings.Join(s.Reasons, "; "))
	} else {
		p("| Search ended at | no step failed |")
	}
	p("| Cluster cost (nodes and disks, list price) | %s an hour, %s a month |", money(m.clusterHourly(), m.Currency), money(m.clusterHourly()*m.HoursPerMonth, m.Currency))
	if c := r.Cost; c != nil {
		p("| Cost per 1K RPS | **%s an hour**, %s a month |", money(c.Per1KRPSHourly, c.Currency), money(c.Per1KRPSMonthly, c.Currency))
		if c.NetPer1KRPSHourly != nil {
			p("| Same, without the load generator's CPU share (estimate) | %s an hour, %s a month (k6 used %.2f of %.2f cores, %s) |",
				money(*c.NetPer1KRPSHourly, c.Currency), money(*c.NetPer1KRPSMonthly, c.Currency), *c.GeneratorCPU, m.allocatableCPU(), pct(*c.GeneratorShare, 1))
		}
	} else {
		p("| Cost per 1K RPS | not computed: no step held every SLO |")
	}
	if r.SuggestedRateLimit != nil {
		p("| Suggested ingress gateway rate limit | %s req/s (%s of the capacity): `tokens_per_fill` in deploy/resilience/ingress-rate-limit.yaml, per gateway replica divided by the replica count |",
			num(*r.SuggestedRateLimit, 0), pct(r.RateLimitShare, 0))
	}
	p("")

	p("## SLOs")
	p("")
	p("| Journey | Availability target | Latency target |")
	p("|---|---:|---:|")
	for _, s := range r.SLOs {
		p("| %s | %s | %s under %s ms |", s.ID, pct(s.Availability.Target, 1), pct(s.Latency.Target, 1), thresholdLabel(s.Latency.ThresholdMs))
	}
	p("")

	p("## Steps")
	p("")
	p("Each journey cell: availability · share of requests under its latency threshold · p95 latency.")
	p("**Bold**: an SLO clearly missed. (?): too few requests to tell, so the step is inconclusive.")
	p("")
	header := "| Step | Planned RPS | Measured req/s | Delivered |"
	align := "|---:|---:|---:|---:|"
	for _, s := range r.SLOs {
		header += " " + s.ID + " |"
		align += "---|"
	}
	p("%s k6 CPU | Node CPU | Nodes | Frontend req/s | Verdict |", header)
	p("%s---:|---:|---:|---:|---|", align)
	for _, s := range r.Steps {
		row := fmt.Sprintf("| %d | %s | %s | %s |", s.Index, num(s.PlannedRPS, -1), optNum(s.MeasuredRPS, 2), optPct(s.Delivered, 0))
		for _, slo := range r.SLOs {
			row += " " + journeyCell(s, slo.ID) + " |"
		}
		row += fmt.Sprintf(" %s | %s | %s | %s | %s |", optNum(s.GeneratorCPU, 2), optPct(s.NodeCPU, 0), optNum(s.Nodes, 0), optNum(s.FrontendRPS, 1), s.Status)
		p("%s", row)
	}
	p("")

	var reasons, warnings []string
	for _, s := range r.Steps {
		if s.Status != statusPass && s.Status != statusNotRun {
			reasons = append(reasons, fmt.Sprintf("- Step %d (%s RPS), %s: %s", s.Index, num(s.PlannedRPS, -1), s.Status, strings.Join(s.Reasons, "; ")))
		}
		for _, w := range s.Warnings {
			warnings = append(warnings, fmt.Sprintf("- Step %d: %s", s.Index, w))
		}
	}
	for _, w := range r.Warnings {
		warnings = append(warnings, "- "+w)
	}
	if len(reasons) > 0 {
		p("## Why steps did not pass")
		p("")
		p("%s", strings.Join(reasons, "\n"))
		p("")
	}
	if len(warnings) > 0 {
		p("## Warnings")
		p("")
		p("%s", strings.Join(warnings, "\n"))
		p("")
	}

	p("## How to read this")
	p("")
	p("- *Measured req/s* counts the requests k6 sent for the journeys. The Locust load generator and the prober")
	p("  kept running; *Frontend req/s* (from the mesh, when available) is everything the frontend served.")
	p("- The SLIs here are measured by k6 at the Istio ingress gateway, per request; failed requests never count")
	p("  as fast. They are a load-test view of the SLOs in `slo/slos.yaml`, not the 28-day SLO itself.")
	p("- A step passes only when every SLI is above its target at the lower end of its Wilson interval, so a")
	p("  lucky few hundred requests cannot pass a 99.5 %% target. Inconclusive steps neither pass nor end the")
	p("  search; fail, invalid and not-run steps end it.")
	p("- Cost per 1K RPS assumes cost grows in proportion to load, on the same nodes at the same list price.")
	p("  It covers nodes and disks only. Source: %s.", m.Source)
	p("- Method and limits: `docs/capacity/README.md` in the repository.")

	_, err := w.Write(b.Bytes())
	return err
}

func journeyCell(s stepResult, id string) string {
	for _, j := range s.Journeys {
		if j.ID != id {
			continue
		}
		if j.Availability == nil {
			return "no requests"
		}
		cell := pct(j.Availability.Value, 3) + " · " + pct(j.Latency.Value, 1)
		if j.P95Ms != nil {
			cell += " · " + num(*j.P95Ms, 0) + " ms"
		}
		switch {
		case j.Availability.Verdict == sliMissed || j.Latency.Verdict == sliMissed:
			cell = "**" + cell + "**"
		case j.Availability.Verdict == sliUndecided || j.Latency.Verdict == sliUndecided:
			cell += " (?)"
		}
		return cell
	}
	return "—"
}

func stepRates(steps []stepResult) []float64 {
	out := make([]float64, len(steps))
	for i, s := range steps {
		out[i] = s.PlannedRPS
	}
	return out
}

func joinNumbers(v []float64) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = num(x, -1)
	}
	return strings.Join(parts, ", ")
}

// num formats with a fixed number of decimals, or the shortest form with decimals < 0.
func num(v float64, decimals int) string { return strconv.FormatFloat(v, 'f', decimals, 64) }

func optNum(v *float64, decimals int) string {
	if v == nil {
		return "n/a"
	}
	return num(*v, decimals)
}

func optPct(v *float64, decimals int) string {
	if v == nil {
		return "n/a"
	}
	return pct(*v, decimals)
}

// money writes $0.1612, $4.05 or $2,957 (US dollars), or "12.30 EUR" for other currencies.
func money(v float64, currency string) string {
	var s string
	switch a := math.Abs(v); {
	case a >= 100:
		s = groupThousands(strconv.FormatFloat(math.Round(v), 'f', 0, 64))
	case a >= 1:
		s = strconv.FormatFloat(v, 'f', 2, 64)
	default:
		s = strconv.FormatFloat(v, 'f', 4, 64)
	}
	if currency == "USD" {
		return "$" + s
	}
	return s + " " + currency
}

func groupThousands(digits string) string {
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return sign + digits
}
