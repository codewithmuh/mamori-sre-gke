package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestScheduleProbesEveryInterval(t *testing.T) {
	shop := newFakeShop(t)
	p, logs := testProber(t, shop, func(c *config) {
		c.interval = 30 * time.Millisecond
		c.scheduled = []string{cujBrowse, cujCheckout}
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	p.startSchedules(ctx, &wg)

	waitFor(t, "three scheduled probes of each journey", func() bool {
		return counterValue(t, p.metrics.probes, cujBrowse, resultSuccess, sourceScheduled) >= 3 &&
			counterValue(t, p.metrics.probes, cujCheckout, resultSuccess, sourceScheduled) >= 3
	})
	cancel()
	wg.Wait()

	if n := counterValue(t, p.metrics.probes, cujBrowse, resultSuccess, sourceOnDemand); n != 0 {
		t.Errorf("on-demand counter = %v, want 0", n)
	}
	if !strings.Contains(logs.String(), `"event":"probe_completed","cuj":"checkout","source":"scheduled","ok":true`) {
		t.Errorf("log has no probe_completed line:\n%s", logs)
	}
	if got := p.stuck(time.Now()); len(got) != 0 {
		t.Errorf("stuck() = %v right after probing", got)
	}
}

func TestScheduleNeverOverlaps(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /", fault{delay: 80 * time.Millisecond})
	p, logs := testProber(t, shop, func(c *config) {
		c.interval = 20 * time.Millisecond
		c.scheduled = []string{cujBrowse}
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	p.startSchedules(ctx, &wg)

	waitFor(t, "three slow probes", func() bool {
		return counterValue(t, p.metrics.probes, cujBrowse, resultSuccess, sourceScheduled) >= 3
	})
	cancel()
	wg.Wait()

	if n := shop.maxInflight.Load(); n != 1 {
		t.Errorf("up to %d probes of one journey ran at once, want 1", n)
	}
	if !strings.Contains(logs.String(), `"event":"probe_skipped","cuj":"browse"`) {
		t.Errorf("log does not report the skipped runs:\n%s", logs)
	}
}

func TestScheduleRecordsFailures(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /product", fault{status: 503, contentType: "text/plain", body: "no healthy upstream"})
	p, logs := testProber(t, shop, func(c *config) {
		c.interval = 20 * time.Millisecond
		c.scheduled = []string{cujBrowse}
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	p.startSchedules(ctx, &wg)

	waitFor(t, "a failed probe", func() bool {
		return counterValue(t, p.metrics.probes, cujBrowse, resultFailure, sourceScheduled) >= 1
	})
	cancel()
	wg.Wait()

	if n := counterValue(t, p.metrics.steps, cujBrowse, "product", resultFailure, sourceScheduled); n < 1 {
		t.Errorf("product step failures = %v", n)
	}
	want := `"failed_step":"product","error":"GET /product/1YMWWN1N4O: status 503, want 200: no healthy upstream"`
	if !strings.Contains(logs.String(), want) || !strings.Contains(logs.String(), `"severity":"WARNING"`) {
		t.Errorf("log lacks %s:\n%s", want, logs)
	}
}

func TestShutdownDropsTheProbeInFlight(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /", fault{delay: 10 * time.Second})
	p, _ := testProber(t, shop, func(c *config) {
		c.interval = time.Second
		c.stepTimeout = 10 * time.Second
		c.scheduled = []string{cujBrowse}
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	p.startSchedules(ctx, &wg)
	waitFor(t, "the first request", func() bool { return slices.Contains(shop.routes(), "GET /") })

	cancel()
	wg.Wait()

	for _, result := range []string{resultSuccess, resultFailure} {
		if n := counterValue(t, p.metrics.probes, cujBrowse, result, sourceScheduled); n != 0 {
			t.Errorf("%s counter = %v after shutdown, want 0", result, n)
		}
	}
}

func TestStuckScheduleFailsLiveness(t *testing.T) {
	shop := newFakeShop(t)
	p, _ := testProber(t, shop, func(c *config) {
		c.interval = time.Minute
		c.stepTimeout = 10 * time.Second
		c.scheduled = []string{cujBrowse, cujCheckout}
	})
	// browse may be quiet for 60 s + 2 x 10 s + 3 s + 30 s = 113 s; checkout for 133 s.
	now := time.Now()
	for _, hb := range p.beats {
		hb.last.Store(now.Add(-2 * time.Minute).UnixNano())
	}
	if got := p.stuck(now); !slices.Equal(got, []string{cujBrowse}) {
		t.Errorf("stuck() = %v, want [browse]", got)
	}

	rec := httptest.NewRecorder()
	p.routes(prometheus.NewRegistry()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "browse") {
		t.Errorf("healthz = %d %q, want 503 naming browse", rec.Code, rec.Body)
	}
}
