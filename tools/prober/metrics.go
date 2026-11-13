package main

import "github.com/prometheus/client_golang/prometheus"

// Label values. SLOs read source="scheduled" only, so on-demand runs (discovery, game days,
// `make prober-run`) never touch an error budget.
const (
	sourceScheduled = "scheduled"
	sourceOnDemand  = "on-demand"
	resultSuccess   = "success"
	resultFailure   = "failure"
)

// durationBuckets in seconds. They include the starting latency thresholds (0.5 s and 1 s,
// docs/prd.md) as exact bounds, so a latency SLI can be read straight from the buckets.
var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2.5, 5, 10, 30}

// metrics are the four series in the build contract, and nothing else: Managed Prometheus bills
// per sample, so the Go runtime and process collectors are left out.
type metrics struct {
	probes        *prometheus.CounterVec
	probeDuration *prometheus.HistogramVec
	steps         *prometheus.CounterVec
	stepDuration  *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mamori_cuj_probe_total",
			Help: "Journey probes by result. A probe fails when any of its steps fails.",
		}, []string{"cuj", "result", "source"}),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mamori_cuj_probe_duration_seconds",
			Help:    "Duration of successful journey probes, all steps together.",
			Buckets: durationBuckets,
		}, []string{"cuj", "source"}),
		steps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mamori_cuj_step_total",
			Help: "Journey steps (one HTTP request each) by result. Steps after a failed step do not run.",
		}, []string{"cuj", "step", "result", "source"}),
		stepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mamori_cuj_step_duration_seconds",
			Help:    "Duration of successful journey steps.",
			Buckets: durationBuckets,
		}, []string{"cuj", "step", "source"}),
	}
	reg.MustRegister(m.probes, m.probeDuration, m.steps, m.stepDuration)
	return m
}

// init creates a journey's series at zero, so failure ratios and absence alerts have data from
// the first scrape instead of from the first failure.
func (m *metrics) init(j journey, source string) {
	m.probeDuration.WithLabelValues(j.id, source)
	for _, result := range []string{resultSuccess, resultFailure} {
		m.probes.WithLabelValues(j.id, result, source)
		for _, st := range j.steps {
			m.steps.WithLabelValues(j.id, st.name, result, source)
		}
	}
	for _, st := range j.steps {
		m.stepDuration.WithLabelValues(j.id, st.name, source)
	}
}

// record counts a probe and its steps. Durations are recorded for successes only: a failed probe
// counts against availability, and a fast error page must not improve the latency SLI.
func (m *metrics) record(r probeResult, source string) {
	m.probes.WithLabelValues(r.cuj, resultLabel(r.ok), source).Inc()
	if r.ok {
		m.probeDuration.WithLabelValues(r.cuj, source).Observe(r.duration.Seconds())
	}
	for _, st := range r.steps {
		m.steps.WithLabelValues(r.cuj, st.name, resultLabel(st.ok), source).Inc()
		if st.ok {
			m.stepDuration.WithLabelValues(r.cuj, st.name, source).Observe(st.duration.Seconds())
		}
	}
}

func resultLabel(ok bool) string {
	if ok {
		return resultSuccess
	}
	return resultFailure
}
