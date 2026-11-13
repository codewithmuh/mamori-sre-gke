package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// maxRuns bounds one on-demand call: 100 checkouts at 4 in parallel take well under a minute on a
// healthy shop (estimate), and at most about 21 minutes if every step times out (25 rounds of
// 4 x 12 s steps plus a 3 s cleanup).
const maxRuns = 100

// Error reasons in API responses.
const (
	reasonInvalid      = "invalid-request"
	reasonUnknownCUJ   = "unknown-cuj"
	reasonBusy         = "busy"
	reasonShuttingDown = "shutting-down"
	reasonCancelled    = "cancelled"
)

type runRequest struct {
	CUJ  string `json:"cuj"`
	Runs *int   `json:"runs"`
}

type runResult struct {
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs"`
	FailedStep string `json:"failedStep"`
	Error      string `json:"error"`
}

type runResponse struct {
	CUJ       string      `json:"cuj"`
	Runs      int         `json:"runs"`
	Successes int         `json:"successes"`
	Failures  int         `json:"failures"`
	Results   []runResult `json:"results"`
}

type apiError struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

func (p *prober) routes(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/run", p.handleRun)
	mux.HandleFunc("GET /healthz", p.handleHealthz)
	mux.HandleFunc("GET /readyz", p.handleReadyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}

// handleRun runs one journey `runs` times now and reports each result. One call per journey at
// a time; calls for different journeys run side by side.
func (p *prober) handleRun(w http.ResponseWriter, r *http.Request) {
	if p.stopping.Load() {
		p.writeError(w, http.StatusServiceUnavailable, reasonShuttingDown, "the prober is shutting down")
		return
	}
	var req runRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		p.writeError(w, http.StatusBadRequest, reasonInvalid, `body must be JSON like {"cuj":"browse","runs":20}: `+err.Error())
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		p.writeError(w, http.StatusBadRequest, reasonInvalid, "body must hold exactly one JSON object")
		return
	}
	j, ok := journeyByID(req.CUJ)
	if !ok {
		p.writeError(w, http.StatusBadRequest, reasonUnknownCUJ,
			fmt.Sprintf("unknown cuj %q; use one of %s", req.CUJ, strings.Join(journeyIDs(), ", ")))
		return
	}
	runs := 1
	if req.Runs != nil {
		runs = *req.Runs
	}
	if runs < 1 || runs > maxRuns {
		p.writeError(w, http.StatusBadRequest, reasonInvalid, fmt.Sprintf("runs must be from 1 to %d", maxRuns))
		return
	}
	if _, running := p.busy.LoadOrStore(j.id, struct{}{}); running {
		p.writeError(w, http.StatusConflict, reasonBusy,
			fmt.Sprintf("an on-demand run of %s is in progress; try again when it ends", j.id))
		return
	}
	defer p.busy.Delete(j.id)

	start := time.Now()
	results, complete := p.runMany(r.Context(), j, runs)
	if !complete {
		reason, msg := reasonCancelled, "the run was cancelled: the caller went away"
		if p.stopping.Load() {
			reason, msg = reasonShuttingDown, "the run was cancelled: the prober is shutting down"
		}
		p.writeError(w, http.StatusServiceUnavailable, reason, msg)
		return
	}

	resp := runResponse{CUJ: j.id, Runs: runs, Results: make([]runResult, runs)}
	for i, res := range results {
		resp.Results[i] = runResult{OK: res.ok, DurationMs: res.duration.Milliseconds(), FailedStep: res.failedStep, Error: res.err}
		if res.ok {
			resp.Successes++
		} else {
			resp.Failures++
			p.log.Debug("on-demand probe failed", "cuj", j.id, "run", i+1, "failed_step", res.failedStep, "error", res.err)
		}
	}
	p.log.Info("on-demand run completed", "event", "on_demand_run_completed", "cuj", j.id, "runs", runs,
		"successes", resp.Successes, "failures", resp.Failures, "duration_ms", time.Since(start).Milliseconds())
	p.writeJSON(w, http.StatusOK, resp)
}

// runMany runs a journey `runs` times, cfg.runConcurrency at a time, each in its own session.
// It reports false if ctx ended first; probes that finished before that are still counted in the
// metrics, because they did happen.
func (p *prober) runMany(ctx context.Context, j journey, runs int) ([]probeResult, bool) {
	results := make([]probeResult, runs)
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(p.cfg.runConcurrency, runs) {
		wg.Go(func() {
			for i := range next {
				res := p.probe(ctx, j)
				if !res.cancelled {
					p.metrics.record(res, sourceOnDemand)
				}
				results[i] = res
			}
		})
	}
feed:
	for i := range runs {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	return results, ctx.Err() == nil
}

func (p *prober) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if ids := p.stuck(time.Now()); len(ids) > 0 {
		http.Error(w, "scheduled probes stopped making progress: "+strings.Join(ids, ", "), http.StatusServiceUnavailable)
		return
	}
	writeText(w, "ok\n")
}

// handleReadyz never looks at the shop: a prober that went unready whenever the shop failed would
// stop reporting exactly when it matters.
func (p *prober) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !p.ready.Load() || p.stopping.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	writeText(w, "ready\n")
}

func (p *prober) writeError(w http.ResponseWriter, status int, reason, msg string) {
	p.writeJSON(w, status, apiError{Error: msg, Reason: reason})
}

func (p *prober) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		p.log.Debug("could not write the response", "error", err.Error())
	}
}

func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, s)
}
