package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// options steer one discovery run.
type options struct {
	CUJs    []string
	Targets []target

	// Runs per (journey, broken service) case.
	Runs int

	// BaselineRuns per journey before any fault; every one must pass.
	BaselineRuns int

	// RecoveryRuns per journey per check while waiting for the shop to recover.
	RecoveryRuns int

	// A journey depends on a service critically when its failure rate with
	// the service broken is at least Threshold.
	Threshold float64

	// Settle is the wait after a fault is in place before measuring, so the
	// new routes reach every sidecar.
	Settle           time.Duration
	RecoveryTimeout  time.Duration
	RecoveryInterval time.Duration

	// RemoveTimeout bounds removing one fault, which also runs after Ctrl-C or
	// a timeout, on a context of its own.
	RemoveTimeout time.Duration
}

// caseResult is what happened to one journey while one service was broken.
type caseResult struct {
	Runs     int
	Failures int
	Steps    map[string]int // failed runs by the step that failed
	Example  string         // the first error message
}

func (c caseResult) rate() float64 {
	if c.Runs == 0 {
		return 0
	}
	return float64(c.Failures) / float64(c.Runs)
}

// results[cuj][service]
type results map[string]map[string]caseResult

func (r results) add(cuj, service string, c caseResult) {
	if r[cuj] == nil {
		r[cuj] = map[string]caseResult{}
	}
	r[cuj][service] = c
}

type discoverer struct {
	opts   options
	faults faultInjector
	prober prober
	log    *slog.Logger
	sleep  func(context.Context, time.Duration) error
}

// discover runs the whole experiment: preflight, baseline, then one fault at a
// time. Each fault is removed before discover moves on or returns, including
// on errors, Ctrl-C and timeouts.
func (d *discoverer) discover(ctx context.Context) (results, error) {
	if err := d.faults.preflight(ctx, d.opts.Targets); err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	d.log.InfoContext(ctx, "baseline: every journey must pass before any fault is injected",
		"cujs", d.opts.CUJs, "runs", d.opts.BaselineRuns)
	failing, err := d.check(ctx, d.opts.BaselineRuns)
	if err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	if len(failing) > 0 {
		return nil, fmt.Errorf("baseline is not clean, so no fault was injected: %s", strings.Join(failing, "; "))
	}
	d.log.InfoContext(ctx, "baseline clean")

	res := results{}
	for i, t := range d.opts.Targets {
		log := d.log.With("service", t.Service, "method", string(t.Method), "target", fmt.Sprintf("%d/%d", i+1, len(d.opts.Targets)))
		if err := d.measure(ctx, t, res, log); err != nil {
			return nil, err
		}
		if err := d.waitRecovered(ctx, t, log); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// measure breaks one service, runs every journey, and repairs the service.
func (d *discoverer) measure(ctx context.Context, t target, res results, log *slog.Logger) (err error) {
	log.InfoContext(ctx, "injecting fault")
	f, err := d.faults.inject(ctx, t)
	if f != nil {
		defer func() {
			// The run's context may be cancelled already (Ctrl-C, timeout);
			// removing the fault must still happen.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.opts.RemoveTimeout)
			defer cancel()
			if rerr := d.faults.remove(rctx, f); rerr != nil {
				err = errors.Join(err, fmt.Errorf("remove fault on %s: %w (run: cuj-discovery cleanup)", t.Service, rerr))
				return
			}
			log.InfoContext(ctx, "fault removed")
		}()
	}
	if err != nil {
		return fmt.Errorf("inject fault on %s: %w", t.Service, err)
	}
	if err := d.sleep(ctx, d.opts.Settle); err != nil {
		return fmt.Errorf("waiting for the fault on %s to take effect: %w", t.Service, err)
	}
	for _, cuj := range d.opts.CUJs {
		r, err := d.prober.Run(ctx, cuj, d.opts.Runs)
		if err != nil {
			return fmt.Errorf("%s with %s broken: %w", cuj, t.Service, err)
		}
		steps, example := failedSteps(r)
		c := caseResult{Runs: r.Runs, Failures: r.Failures, Steps: steps, Example: example}
		res.add(cuj, t.Service, c)
		if n := rateLimited(r); n > 0 {
			// The gateway's rate limit, not the fault, failed these runs.
			log.WarnContext(ctx, "runs failed on the gateway's rate limit; this case over-counts failures, re-run with fewer -runs",
				"cuj", cuj, "rate_limited_runs", n)
		}
		level := slog.LevelInfo
		if c.rate() > 0 && c.rate() < 1 {
			// Neither always nor never: flaky, conditional (a step that only
			// sometimes calls the service), or the fault took effect late.
			level = slog.LevelWarn
		}
		log.Log(ctx, level, "case measured", "cuj", cuj, "runs", c.Runs, "failures", c.Failures,
			"failure_rate", c.rate(), "critical", c.rate() >= d.opts.Threshold, "failed_steps", steps, "example_error", example)
	}
	if err := d.faults.verify(ctx, f); err != nil {
		return fmt.Errorf("the fault on %s did not hold for the whole measurement, so its results are not valid: %w", t.Service, err)
	}
	return nil
}

// waitRecovered waits until every journey passes again after a fault is
// removed: new routes take a moment to reach every sidecar, and a restored
// redis-cart needs to start and pass its readiness probe. (cartservice itself
// stays Ready throughout: in v0.10.7 its health check never touches Redis.)
// Measuring the next service before then would blame it for the previous
// one's damage.
func (d *discoverer) waitRecovered(ctx context.Context, t target, log *slog.Logger) error {
	rctx, cancel := context.WithTimeout(ctx, d.opts.RecoveryTimeout)
	defer cancel()
	for attempt := 1; ; attempt++ {
		failing, err := d.check(rctx, d.opts.RecoveryRuns)
		if err == nil && len(failing) == 0 {
			log.InfoContext(ctx, "journeys pass again", "attempts", attempt)
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for the shop to recover from the fault on %s: %w", t.Service, ctx.Err())
		}
		if err != nil {
			failing = append(failing, err.Error())
		}
		log.WarnContext(ctx, "journeys have not recovered yet", "attempt", attempt, "failing", failing)
		if serr := d.sleep(rctx, d.opts.RecoveryInterval); serr != nil {
			return fmt.Errorf("journeys still fail %s after the fault on %s was removed, so later results would be wrong: %s",
				d.opts.RecoveryTimeout, t.Service, strings.Join(failing, "; "))
		}
	}
}

// check runs every journey and describes the ones with a failed run.
func (d *discoverer) check(ctx context.Context, runs int) ([]string, error) {
	var failing []string
	for _, cuj := range d.opts.CUJs {
		r, err := d.prober.Run(ctx, cuj, runs)
		if err != nil {
			return nil, err
		}
		if r.Failures > 0 {
			steps, example := failedSteps(r)
			failing = append(failing, fmt.Sprintf("%s: %d/%d failed (steps %v; first error: %s)", cuj, r.Failures, r.Runs, steps, example))
		}
	}
	return failing, nil
}
