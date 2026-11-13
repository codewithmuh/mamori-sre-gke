package main

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsInitCreatesZeroSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	for _, j := range journeys {
		m.init(j, sourceScheduled)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	// 11 (journey, step) pairs across the four journeys.
	want := map[string]int{
		"mamori_cuj_probe_total":            4 * 2,
		"mamori_cuj_probe_duration_seconds": 4,
		"mamori_cuj_step_total":             11 * 2,
		"mamori_cuj_step_duration_seconds":  11,
	}
	for _, f := range families {
		if got := len(f.GetMetric()); got != want[f.GetName()] {
			t.Errorf("%s has %d series, want %d", f.GetName(), got, want[f.GetName()])
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "source" && l.GetValue() != sourceScheduled {
					t.Errorf("%s has source %q", f.GetName(), l.GetValue())
				}
			}
			if metric.GetCounter().GetValue() != 0 || metric.GetHistogram().GetSampleCount() != 0 {
				t.Errorf("%s starts at non-zero", f.GetName())
			}
		}
		delete(want, f.GetName())
	}
	if len(want) != 0 {
		t.Errorf("missing families: %v", want)
	}
}

func TestMetricsRecord(t *testing.T) {
	m := newMetrics(prometheus.NewRegistry())
	m.record(probeResult{cuj: cujCheckout, ok: true, duration: 800 * time.Millisecond, steps: []stepResult{
		{"home", true, 100 * time.Millisecond},
		{"add-to-cart", true, 100 * time.Millisecond},
		{"view-cart", true, 200 * time.Millisecond},
		{"place-order", true, 400 * time.Millisecond},
	}}, sourceScheduled)
	m.record(probeResult{cuj: cujCheckout, ok: false, duration: 300 * time.Millisecond, failedStep: "add-to-cart", steps: []stepResult{
		{"home", true, 100 * time.Millisecond},
		{"add-to-cart", false, 200 * time.Millisecond},
	}}, sourceOnDemand)

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"scheduled successes", counterValue(t, m.probes, cujCheckout, resultSuccess, sourceScheduled), 1},
		{"scheduled failures", counterValue(t, m.probes, cujCheckout, resultFailure, sourceScheduled), 0},
		{"on-demand failures", counterValue(t, m.probes, cujCheckout, resultFailure, sourceOnDemand), 1},
		{"scheduled probe durations", float64(histogramCount(t, m.probeDuration, cujCheckout, sourceScheduled)), 1},
		{"on-demand probe durations (failures only)", float64(histogramCount(t, m.probeDuration, cujCheckout, sourceOnDemand)), 0},
		{"on-demand home successes", counterValue(t, m.steps, cujCheckout, "home", resultSuccess, sourceOnDemand), 1},
		{"on-demand add-to-cart failures", counterValue(t, m.steps, cujCheckout, "add-to-cart", resultFailure, sourceOnDemand), 1},
		{"on-demand view-cart never ran", counterValue(t, m.steps, cujCheckout, "view-cart", resultFailure, sourceOnDemand), 0},
		{"on-demand add-to-cart durations", float64(histogramCount(t, m.stepDuration, cujCheckout, "add-to-cart", sourceOnDemand)), 0},
		{"scheduled place-order durations", float64(histogramCount(t, m.stepDuration, cujCheckout, "place-order", sourceScheduled)), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestBucketsHoldTheLatencyThresholds(t *testing.T) {
	var bounds []string
	for _, b := range durationBuckets {
		bounds = append(bounds, time.Duration(b*float64(time.Second)).String())
	}
	got := strings.Join(bounds, " ")
	for _, threshold := range []string{" 500ms ", " 1s "} {
		if !strings.Contains(" "+got+" ", threshold) {
			t.Errorf("buckets %s lack the SLO threshold%s", got, threshold)
		}
	}
}
