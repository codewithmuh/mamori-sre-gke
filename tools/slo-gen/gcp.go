package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// obj is a JSON object of Terraform's JSON syntax.
type obj = map[string]any

const pathLabTF = "infra/envs/lab/slo_generated.tf.json"

// Cloud Monitoring metric types of Managed Prometheus data:
// prometheus.googleapis.com/<name>/<kind>. Counters are CUMULATIVE DOUBLE,
// histograms CUMULATIVE DISTRIBUTION, and recording-rule results GAUGE.
func gmpType(metric, kind string) string { return "prometheus.googleapis.com/" + metric + "/" + kind }

// monFilter joins Cloud Monitoring filter terms. Values are quoted with
// strconv-style escaping via %q, which the filter language accepts.
func monFilter(terms ...string) string { return strings.Join(terms, " AND ") }

func eqTerm(field, value string) string { return fmt.Sprintf("%s = %q", field, value) }

func oneOfTerm(field string, values []string) string {
	if len(values) == 1 {
		return eqTerm(field, values[0])
	}
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = fmt.Sprintf("%q", v)
	}
	return fmt.Sprintf("%s = one_of(%s)", field, strings.Join(q, ", "))
}

func (m *Model) proberFilter(metricType string, j Journey, extra ...string) string {
	terms := []string{
		eqTerm("metric.type", metricType),
		eqTerm("resource.type", "prometheus_target"),
		eqTerm("resource.labels.cluster", m.Spec.GCP.Cluster),
		eqTerm("resource.labels.namespace", m.Spec.Prober.Namespace),
		eqTerm("metric.labels.source", "scheduled"),
		eqTerm("metric.labels.cuj", j.ID),
	}
	return monFilter(append(terms, extra...)...)
}

func (m *Model) recordedFilter(record string, j Journey) string {
	return monFilter(
		eqTerm("metric.type", gmpType(record, "gauge")),
		eqTerm("resource.type", "prometheus_target"),
		eqTerm("resource.labels.cluster", m.Spec.GCP.Cluster),
		eqTerm("metric.labels.cuj", j.ID),
	)
}

// baseLabels are the repository's labels for every resource that takes them.
func baseLabels() obj {
	return obj{"app": "mamori", "env": "lab", "managed-by": "terraform", "generator": "slo-gen"}
}

func withLabels(extra obj) obj {
	l := baseLabels()
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func serviceID(j Journey) string { return "mamori-cuj-" + j.ID }

// sloSLI returns the SLI block of one SLO.
//   - prober availability: request-based, good runs / all runs (counters).
//   - prober latency: request-based distribution cut on the step histogram.
//   - mesh: windows-based on the recorded composite gauge. Cloud Monitoring
//     ratios take only two filters, so a product over several services can
//     only come in as a precomputed series; a gauge only fits a windows-based
//     SLI. A window is good when the composite meets the target in it.
func (m *Model) sloSLI(j Journey, s SLO) (string, any) {
	switch {
	case s.Signal == signalProber && s.SLI == sliAvailability:
		total := m.proberFilter(gmpType(metricProbeTotal, "counter"), j)
		good := m.proberFilter(gmpType(metricProbeTotal, "counter"), j, eqTerm("metric.labels.result", "success"))
		return "request_based_sli", []obj{{"good_total_ratio": []obj{{"good_service_filter": good, "total_service_filter": total}}}}
	case s.Signal == signalProber:
		f := m.proberFilter(gmpType(metricStepDuration, "histogram"), j, oneOfTerm("metric.labels.step", j.ProberSteps))
		return "request_based_sli", []obj{{"distribution_cut": []obj{{
			"distribution_filter": f,
			"range":               []obj{{"max": float64(j.Latency.ThresholdMs) / 1000}},
		}}}}
	default:
		// The upper bound only has to be above any ratio; 2 leaves room for float error.
		return "windows_based_sli", []obj{{
			"window_period": m.Spec.Mesh.WindowPeriod.Seconds(),
			"metric_mean_in_range": []obj{{
				"time_series": m.recordedFilter(recCUJ(signalMesh, s.SLI, m.Spec.Mesh.SLORateWindow), j),
				"range":       []obj{{"min": s.Target, "max": 2}},
			}},
		}}
	}
}

func sloDescription(j Journey, s SLO) string {
	switch {
	case s.Signal == signalProber && s.SLI == sliAvailability:
		return "scheduled prober runs that succeed"
	case s.Signal == signalProber:
		return fmt.Sprintf("successful prober steps (%s) under %d ms", strings.Join(j.ProberSteps, ", "), j.Latency.ThresholdMs)
	case s.SLI == sliAvailability:
		return "minutes in which the product of the critical services' success ratios meets the target"
	default:
		return fmt.Sprintf("minutes in which every critical service has at least the target share of requests under %d ms", j.Latency.ThresholdMs)
	}
}

// sloDisplayName is short: the console lists SLOs by it. The catalog has the long form.
func sloDisplayName(m *Model, j Journey, s SLO) string {
	name := fmt.Sprintf("CUJ %s: %s %s %s %%", j.ID, s.Signal, s.SLI, percent(s.Target))
	if s.Signal == signalMesh {
		name += " of " + m.Spec.Mesh.WindowPeriod.String() + " windows"
	}
	if s.SLI == sliLatency {
		name += fmt.Sprintf(" under %d ms", j.Latency.ThresholdMs)
	}
	return name
}

func (m *Model) sloResource(j Journey, s SLO) obj {
	key, sli := m.sloSLI(j, s)
	return obj{
		"service":             fmt.Sprintf("${google_monitoring_custom_service.%s.service_id}", tfName(j.ID)),
		"slo_id":              s.ID(),
		"display_name":        sloDisplayName(m, j, s),
		"goal":                s.Target,
		"rolling_period_days": int(m.Spec.Window.D().Hours() / 24),
		"user_labels":         withLabels(obj{"cuj": j.ID, "signal": s.Signal, "sli": s.SLI}),
		key:                   sli,
	}
}

// confirmQuery is the PromQL a responder runs to see the SLI right now.
func (m *Model) confirmQuery(j Journey, s SLO) string {
	w := m.Spec.Prober.RecordWindows[0]
	if s.Signal == signalMesh {
		w = m.Spec.Mesh.RecordWindows[0]
	}
	return selector(recCUJ(s.Signal, s.SLI, w), matcher("cuj", "=", j.ID))
}

func (m *Model) alertDoc(j Journey, s SLO, t TierSpec, burn float64) obj {
	runbook := m.RunbookURL(j.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "**Runbook:** [docs/runbooks/cuj-%s.md](%s)\n\n", j.ID, runbook)
	fmt.Fprintf(&b, "The **%s** journey is spending its %s error budget at least %s times faster than the %s window allows "+
		"(%s of the budget in %s), over both the last %s and the last %s.\n\n",
		j.ID, s.SLI, formatFloat(burn), m.Spec.Window, budgetShare(t), t.LongWindow, t.LongWindow, t.ShortWindow)
	fmt.Fprintf(&b, "- SLO: %s %% of %s, rolling %s (`%s` in service `%s`).\n", percent(s.Target), sloDescription(j, s), m.Spec.Window, s.ID(), serviceID(j))
	if s.Signal == signalProber {
		b.WriteString("- Signal: the black-box prober (`tools/prober`), one run a minute.\n")
	} else {
		fmt.Fprintf(&b, "- Signal: Istio client-side metrics of %s.\n", strings.Join(j.MeshServices, ", "))
	}
	fmt.Fprintf(&b, "- Critical services from the CUJ map: %s.\n", listOrNone(j.Critical))
	fmt.Fprintf(&b, "- Check now (PromQL): `%s` (target %s).\n", m.confirmQuery(j, s), formatFloat(s.Target))
	if s.Signal == signalMesh {
		fmt.Fprintf(&b, "- Per service (PromQL): `%s`.\n", m.serviceQuery(j, s))
	}
	fmt.Fprintf(&b, "- Severity: %s. Alert tier `%s`.\n\n", t.Severity, t.Name)
	b.WriteString("Generated by slo-gen from slo/slos.yaml; change the SLO there, not here.\n")
	return obj{
		"mime_type": "text/markdown",
		"subject":   fmt.Sprintf("[%s] CUJ %s: %s %s %s burn", strings.ToUpper(t.Severity), j.ID, s.Signal, s.SLI, t.Name),
		"content":   b.String(),
		"links":     []obj{{"display_name": "Runbook: CUJ " + j.ID, "url": runbook}},
	}
}

func (m *Model) serviceQuery(j Journey, s SLO) string {
	w := m.Spec.Mesh.RecordWindows[0]
	ms := []string{matcher(serviceLabel, "=~", anyOf(j.MeshServices))}
	if s.SLI == sliLatency {
		ms = append([]string{matcher("threshold_ms", "=", fmt.Sprint(j.Latency.ThresholdMs))}, ms...)
	}
	return selector(recService(s.SLI, w), ms...)
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func (m *Model) channels(severity string) string {
	if severity == severityPage {
		return "${local.incident_notification_channels}"
	}
	return "${local.incident_ticket_notification_channels}"
}

func gcpSeverity(severity string) string {
	if severity == severityPage {
		return "CRITICAL"
	}
	return "WARNING"
}

func (m *Model) burnCondition(sloRef string, t TierSpec, lookback Duration, burn float64) obj {
	return obj{
		"display_name": fmt.Sprintf("%s burn: %s lookback", t.Name, lookback),
		"condition_threshold": []obj{{
			"filter":          fmt.Sprintf("select_slo_burn_rate(\"${google_monitoring_slo.%s.name}\", %q)", sloRef, lookback.Seconds()),
			"comparison":      "COMPARISON_GT",
			"threshold_value": burn,
			"duration":        "0s",
			"trigger":         []obj{{"count": 1}},
		}},
	}
}

// burnPolicy is one tier of one SLO: two conditions (long and short lookback)
// that must both hold, the workbook's multiwindow rule. The short window makes
// the alert stop soon after the burn stops.
func (m *Model) burnPolicy(j Journey, s SLO, t TierSpec) obj {
	burn := BurnRate(t, m.Spec.Window)
	sloRef := tfName(j.ID, s.Signal, s.SLI)
	return obj{
		"display_name":          fmt.Sprintf("CUJ %s: %s %s %s burn", j.ID, s.Signal, s.SLI, t.Name),
		"combiner":              "AND",
		"severity":              gcpSeverity(t.Severity),
		"notification_channels": m.channels(t.Severity),
		"user_labels": withLabels(obj{
			"cuj": j.ID, "signal": s.Signal, "sli": s.SLI,
			"alert": "burn-rate", "burn": t.Name, "severity": t.Severity,
		}),
		"alert_strategy": []obj{{"auto_close": m.Spec.Alerting.AutoClose.Seconds()}},
		"documentation":  []obj{m.alertDoc(j, s, t, burn)},
		"conditions": []obj{
			m.burnCondition(sloRef, t, t.LongWindow, burn),
			m.burnCondition(sloRef, t, t.ShortWindow, burn),
		},
	}
}

// noDataPolicy pages when a journey's prober series stop arriving: the prober,
// its node, Managed Prometheus or the whole cluster is down. Burn-rate alerts
// see no data in that case and stay silent.
func (m *Model) noDataPolicy(j Journey) obj {
	runbook := m.RunbookURL(j.ID)
	after := m.Spec.Alerting.NoDataAfter
	content := fmt.Sprintf("**Runbook:** [docs/runbooks/cuj-%s.md](%s)\n\n"+
		"No prober data for the **%s** journey for %s. The SLOs of this journey are blind until it returns: "+
		"check the prober (`kubectl -n %s get pods`), the nodes (the spend guard may have scaled them to zero) "+
		"and Managed Prometheus collection.\n\n"+
		"Generated by slo-gen from slo/slos.yaml; change it there, not here.\n",
		j.ID, runbook, j.ID, after, m.Spec.Prober.Namespace)
	return obj{
		"display_name":          fmt.Sprintf("CUJ %s: prober no data", j.ID),
		"combiner":              "OR",
		"severity":              gcpSeverity(severityPage),
		"notification_channels": m.channels(severityPage),
		"user_labels": withLabels(obj{
			"cuj": j.ID, "signal": signalProber, "sli": sliAvailability,
			"alert": "no-data", "burn": "none", "severity": severityPage,
		}),
		"alert_strategy": []obj{{"auto_close": m.Spec.Alerting.AutoClose.Seconds()}},
		"documentation": []obj{{
			"mime_type": "text/markdown",
			"subject":   fmt.Sprintf("[PAGE] CUJ %s: prober no data", j.ID),
			"content":   content,
			"links":     []obj{{"display_name": "Runbook: CUJ " + j.ID, "url": runbook}},
		}},
		"conditions": []obj{{
			"display_name": fmt.Sprintf("no scheduled prober data for %s", after),
			"condition_absent": []obj{{
				"filter":   m.proberFilter(gmpType(metricProbeTotal, "counter"), j),
				"duration": after.Seconds(),
				"aggregations": []obj{{
					"alignment_period":     "60s",
					"per_series_aligner":   "ALIGN_RATE",
					"cross_series_reducer": "REDUCE_SUM",
					"group_by_fields":      []string{"metric.label.cuj"},
				}},
				"trigger": []obj{{"count": 1}},
			}},
		}},
	}
}

func (m *Model) renderLab() (File, error) {
	services := obj{}
	slos := obj{}
	policies := obj{}
	for _, j := range m.Journeys {
		services[tfName(j.ID)] = obj{
			"service_id":   serviceID(j),
			"display_name": "CUJ " + j.ID,
			"user_labels":  withLabels(obj{"cuj": j.ID}),
		}
		for _, s := range j.SLOs {
			slos[tfName(j.ID, s.Signal, s.SLI)] = m.sloResource(j, s)
			for _, t := range m.TiersFor(s.Signal) {
				policies[tfName(j.ID, s.Signal, s.SLI, t.Name)] = m.burnPolicy(j, s, t)
			}
		}
		policies[tfName(j.ID, "prober", "no_data")] = m.noDataPolicy(j)
	}
	dash, err := m.dashboardJSON()
	if err != nil {
		return File{}, err
	}
	root := obj{
		"//": m.Header(),
		"resource": obj{
			"google_monitoring_custom_service": services,
			"google_monitoring_slo":            slos,
			"google_monitoring_alert_policy":   policies,
			"google_monitoring_dashboard":      obj{"slo_overview": obj{"dashboard_json": dash}},
		},
	}
	b, err := encodeJSON(root)
	return File{Path: pathLabTF, Content: b}, err
}

// encodeJSON writes indented JSON with sorted keys (encoding/json sorts map
// keys), so the output is stable, and without HTML escaping, so PromQL and
// Markdown stay readable.
func encodeJSON(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
