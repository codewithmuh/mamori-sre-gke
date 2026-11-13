package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Names of the raw metrics, fixed by the build contract.
const (
	metricProbeTotal   = "mamori_cuj_probe_total"
	metricStepDuration = "mamori_cuj_step_duration_seconds"
	metricIstioReqs    = "istio_requests_total"
	metricIstioLatency = "istio_request_duration_milliseconds"
)

// Recorded series. The level:metric:operations names follow the Prometheus
// recording-rule convention; "mamori" is the level because the series carry
// the journey (cuj) or the service (destination_service_name) as a label.
func recCUJ(signal, sli string, w Duration) string {
	return fmt.Sprintf("mamori:cuj_%s_%s:ratio_rate%s", signal, sli, w)
}

func recService(sli string, w Duration) string {
	return fmt.Sprintf("mamori:service_mesh_%s:ratio_rate%s", sli, w)
}

// matcher builds one PromQL label matcher; values are quoted Go-style, which
// is what PromQL string literals accept.
func matcher(label, op, value string) string { return label + op + strconv.Quote(value) }

func selector(metric string, matchers ...string) string {
	return metric + "{" + strings.Join(matchers, ",") + "}"
}

// anyOf is a regular expression that matches exactly one of the values.
func anyOf(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = regexp.QuoteMeta(v)
	}
	return strings.Join(q, "|")
}

// leMatch matches a histogram bucket bound however it is formatted: "0.5",
// "0.50", "1", "1.0". Prometheus 3 normalises le values to floats ("1.0"),
// older scrapers and exporters may not.
func leMatch(bound float64) string {
	s := formatFloat(bound)
	if strings.Contains(s, ".") {
		return regexp.QuoteMeta(s) + "0*"
	}
	return regexp.QuoteMeta(s) + `(\.0*)?`
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// ratio is good/total per group over a rate window. A group with traffic but
// no good series yet reads 0 instead of disappearing (or 0 * total), and a
// group without traffic disappears instead of dividing by zero (> 0).
func ratio(by, good, total string, w Duration) string {
	return fmt.Sprintf(`(
  sum by (%[1]s) (rate(%[2]s[%[4]s]))
  or
  0 * sum by (%[1]s) (rate(%[3]s[%[4]s]))
)
/
(sum by (%[1]s) (rate(%[3]s[%[4]s])) > 0)
`, by, good, total, w)
}

// proberAvailabilityExpr: share of scheduled runs of the journey that succeeded.
func proberAvailabilityExpr(j Journey, w Duration) string {
	base := []string{matcher("source", "=", "scheduled"), matcher("cuj", "=", j.ID)}
	good := selector(metricProbeTotal, append(base, matcher("result", "=", "success"))...)
	return ratio("cuj", good, selector(metricProbeTotal, base...), w)
}

// proberLatencyExpr: share of the journey's own (successful) steps that were
// faster than the threshold.
func proberLatencyExpr(j Journey, w Duration) string {
	base := []string{matcher("source", "=", "scheduled"), matcher("cuj", "=", j.ID), stepMatcher(j.ProberSteps)}
	good := selector(metricStepDuration+"_bucket", append(base, matcher("le", "=~", leMatch(float64(j.Latency.ThresholdMs)/1000)))...)
	return ratio("cuj", good, selector(metricStepDuration+"_count", base...), w)
}

func stepMatcher(steps []string) string {
	if len(steps) == 1 {
		return matcher("step", "=", steps[0])
	}
	return matcher("step", "=~", anyOf(steps))
}

// serviceLabel identifies the called service. Not destination_workload: when
// the client's sidecar answers by itself (an injected abort, no healthy
// upstream), no server took part and the workload is reported as "unknown",
// while the service name comes from the route and is always set.
const serviceLabel = "destination_service_name"

// meshBase selects client-side (reporter="source") Istio metrics of the given
// services. The client's sidecar sees what the caller sees, including Istio
// fault injection, which aborts or delays a request before it leaves the
// client, so the server's sidecar never records it.
func (m *Model) meshBase(services []string) []string {
	return []string{
		matcher("reporter", "=", "source"),
		matcher("destination_service_namespace", "=", m.Spec.Mesh.Namespace),
		matcher(serviceLabel, "=~", anyOf(services)),
	}
}

// serviceAvailabilityExpr: per service, the share of requests that got a
// response below 500 and gRPC status OK (or no gRPC status, for plain HTTP).
// response_code 0 (no response at all) is bad.
func (m *Model) serviceAvailabilityExpr(w Duration) string {
	base := m.meshBase(m.Services)
	good := selector(metricIstioReqs, append(base,
		matcher("response_code", "=~", "[1-4][0-9][0-9]"),
		matcher("grpc_response_status", "=~", "0|"))...)
	return ratio(serviceLabel, good, selector(metricIstioReqs, base...), w)
}

// serviceLatencyExpr: per service, the share of requests faster than the threshold.
func (m *Model) serviceLatencyExpr(thresholdMs int, services []string, w Duration) string {
	base := m.meshBase(services)
	good := selector(metricIstioLatency+"_bucket", append(base, matcher("le", "=~", leMatch(float64(thresholdMs))))...)
	return ratio(serviceLabel, good, selector(metricIstioLatency+"_count", base...), w)
}

// cujMeshAvailabilityExpr is D4's availability: the product of the journey's
// critical services' success ratios. PromQL has no product aggregation, so it
// is exp(sum(ln(x))). ln(0) is -Inf and exp(-Inf) is 0, so one service at
// zero makes the journey zero. A service without traffic in the window drops
// out (counts as 1); if all do, the series is absent and the window is empty.
func cujMeshAvailabilityExpr(j Journey, w Duration) string {
	sel := selector(recService(sliAvailability, w), matcher(serviceLabel, "=~", anyOf(j.MeshServices)))
	return fmt.Sprintf("exp(sum(ln(\n  %s\n)))\n", sel)
}

// cujMeshLatencyExpr is D4's latency: the lowest achievement rate among the
// journey's critical services, at the journey's threshold.
func cujMeshLatencyExpr(j Journey, w Duration) string {
	sel := selector(recService(sliLatency, w),
		matcher("threshold_ms", "=", strconv.Itoa(j.Latency.ThresholdMs)),
		matcher(serviceLabel, "=~", anyOf(j.MeshServices)))
	return fmt.Sprintf("min(\n  %s\n)\n", sel)
}

// windows returns the distinct windows, in the order given, with extra first.
func windows(extra []Duration, ws []Duration) []Duration {
	var out []Duration
	seen := map[Duration]bool{}
	for _, w := range append(append([]Duration{}, extra...), ws...) {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// meshWindows: the SLO's rate window first, then the recorded ones.
func (m *Model) meshWindows() []Duration {
	return windows([]Duration{m.Spec.Mesh.SLORateWindow}, m.Spec.Mesh.RecordWindows)
}
