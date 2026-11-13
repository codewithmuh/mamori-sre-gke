package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are scraped by Managed Service for Prometheus (PodMonitoring in deploy/executor).
// Label values come from fixed lists, so a bad request cannot create new series.
type metrics struct {
	events         *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	enabled        prometheus.Gauge
	pending        prometheus.Gauge
	slackConnected prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mamori_executor_events_total",
			Help: "Audit events by event, action type and reason (blocked = refused by a guard).",
		}, []string{"event", "action", "reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mamori_executor_action_duration_seconds",
			Help:    "Time the Kubernetes call of an approved action took, by action type and result.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"action", "result"}),
		enabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mamori_executor_enabled",
			Help: "1 when the kill switch allows actions (executor-config enabled=\"true\" and readable), else 0.",
		}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mamori_executor_pending_proposals",
			Help: "Proposals waiting for a decision in Slack.",
		}),
		slackConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mamori_executor_slack_connected",
			Help: "1 while the Slack Socket Mode connection is up, else 0.",
		}),
	}
	reg.MustRegister(m.events, m.duration, m.enabled, m.pending, m.slackConnected)
	return m
}

var (
	knownActions = map[string]bool{actionRestart: true, actionScale: true, actionRollback: true, actionRemoveFault: true}
	knownReasons = map[string]bool{
		reasonNotAllowed: true, reasonOutOfBounds: true, reasonAlreadyActed: true, reasonKillSwitch: true,
		reasonInvalid: true, reasonUnauthorized: true, reasonApprovalUnavailable: true, reasonExpired: true,
		reasonExecError: true, reasonInterrupted: true, reasonInternal: true,
		reasonTooManyOpen: true, reasonRateLimited: true, reasonConflict: true,
		"not-approver": true, "message-mismatch": true, "content-mismatch": true, "unknown-proposal": true, "config-unreadable": true,
	}
)

func bounded(v string, known map[string]bool) string {
	switch {
	case v == "":
		return "none"
	case known[v]:
		return v
	default:
		return "other"
	}
}

func (m *metrics) event(event, action, reason string) {
	m.events.WithLabelValues(event, bounded(action, knownActions), bounded(reason, knownReasons)).Inc()
}

func (m *metrics) observe(action, result string, d time.Duration) {
	m.duration.WithLabelValues(bounded(action, knownActions), result).Observe(d.Seconds())
}

func (m *metrics) setEnabled(on bool) { m.enabled.Set(b2f(on)) }
func (m *metrics) setPending(n int)   { m.pending.Set(float64(n)) }
func (m *metrics) setSlack(up bool)   { m.slackConnected.Set(b2f(up)) }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
