package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type prober struct {
	cfg     config
	metrics *metrics
	log     *slog.Logger
	pick    func(n int) int // which product a shopper looks at

	beats []heartbeat // one per scheduled journey, fixed before the server starts
	// runJars holds the on-demand sessions: per journey, one cookie jar per worker slot, reused
	// across calls. Fixed at start; a call only uses its own journey's slots, one per worker.
	runJars  map[string][]http.CookieJar
	busy     sync.Map // journey id -> struct{}: an on-demand run is in progress
	ready    atomic.Bool
	stopping atomic.Bool
}

// heartbeat is when a scheduled loop last showed progress (Unix nanoseconds).
type heartbeat struct {
	journey journey
	last    *atomic.Int64
}

func newProber(cfg config, m *metrics, log *slog.Logger) *prober {
	p := &prober{
		cfg:     cfg,
		metrics: m,
		log:     log,
		pick:    func(n int) int { return rand.IntN(n) }, //nolint:gosec // G404: picks a product to look at, not a secret
		runJars: map[string][]http.CookieJar{},
	}
	for _, j := range journeys {
		for range cfg.runConcurrency {
			p.runJars[j.id] = append(p.runJars[j.id], newJar())
		}
	}
	now := time.Now().UnixNano()
	for _, id := range cfg.scheduled {
		j, _ := journeyByID(id) // loadConfig checked the ids
		last := &atomic.Int64{}
		last.Store(now)
		p.beats = append(p.beats, heartbeat{journey: j, last: last})
	}
	return p
}

// startSchedules starts one loop per scheduled journey. The loops start spread over one interval,
// so the journeys don't all hit the shop in the same second.
func (p *prober) startSchedules(ctx context.Context, wg *sync.WaitGroup) {
	n := time.Duration(len(p.beats))
	for i, hb := range p.beats {
		p.metrics.init(hb.journey, sourceScheduled)
		offset := p.cfg.interval * time.Duration(i) / n
		wg.Go(func() { p.runSchedule(ctx, hb, offset) })
	}
}

// runSchedule probes one journey every interval until ctx ends, always as the same shopper (one
// cookie jar for the loop). A probe never overlaps the next one of the same journey: if a probe
// overruns, the runs it missed are skipped, not caught up.
func (p *prober) runSchedule(ctx context.Context, hb heartbeat, offset time.Duration) {
	jar := newJar()
	next := time.Now().Add(offset)
	timer := time.NewTimer(offset)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		hb.last.Store(time.Now().UnixNano())
		res := p.probe(ctx, hb.journey, jar)
		if res.cancelled {
			return
		}
		p.metrics.record(res, sourceScheduled)
		p.logResult(res, sourceScheduled)

		now := time.Now()
		hb.last.Store(now.UnixNano())
		next = next.Add(p.cfg.interval)
		if now.After(next) {
			missed := now.Sub(next)/p.cfg.interval + 1
			next = next.Add(missed * p.cfg.interval)
			p.log.Warn("probe took longer than the interval; skipping the runs it overlapped",
				"event", "probe_skipped", "cuj", hb.journey.id, "skipped", int64(missed))
		}
		timer.Reset(time.Until(next))
	}
}

// stuck returns the scheduled journeys whose loop has shown no progress for longer than a full
// interval plus the longest possible probe (every step and the cleanup timing out), plus slack.
func (p *prober) stuck(now time.Time) []string {
	var ids []string
	for _, hb := range p.beats {
		longest := time.Duration(len(hb.journey.steps))*p.cfg.stepTimeout + cleanupTimeout
		limit := p.cfg.interval + longest + 30*time.Second
		if now.Sub(time.Unix(0, hb.last.Load())) > limit {
			ids = append(ids, hb.journey.id)
		}
	}
	return ids
}

func (p *prober) logResult(r probeResult, source string) {
	steps := make([]any, 0, len(r.steps))
	for _, st := range r.steps {
		steps = append(steps, slog.Int64(st.name, st.duration.Milliseconds()))
	}
	attrs := []any{
		"event", "probe_completed",
		"cuj", r.cuj,
		"source", source,
		"ok", r.ok,
		"duration_ms", r.duration.Milliseconds(),
		slog.Group("steps_ms", steps...),
	}
	if r.ok {
		p.log.Info("probe succeeded", attrs...)
		return
	}
	attrs = append(attrs, "failed_step", r.failedStep, "error", r.err)
	p.log.Warn("probe failed", attrs...)
}
