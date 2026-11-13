package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func postRun(t *testing.T, ctx context.Context, p *prober, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/run", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.routes(prometheus.NewRegistry()).ServeHTTP(rec, req)
	return rec
}

func TestRunOnDemand(t *testing.T) {
	shop := newFakeShop(t)
	p, logs := testProber(t, shop)

	rec := postRun(t, t.Context(), p, `{"cuj":"checkout","runs":3}`)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	// The exact shape from the build contract.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(raw); !slices.Equal(got, []string{"cuj", "failures", "results", "runs", "successes"}) {
		t.Errorf("response keys = %v", got)
	}
	var results []map[string]json.RawMessage
	if err := json.Unmarshal(raw["results"], &results); err != nil || len(results) != 3 {
		t.Fatalf("results = %s (%v)", raw["results"], err)
	}
	if got := sortedKeys(results[0]); !slices.Equal(got, []string{"durationMs", "error", "failedStep", "ok"}) {
		t.Errorf("result keys = %v", got)
	}

	var resp runResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.CUJ != cujCheckout || resp.Runs != 3 || resp.Successes != 3 || resp.Failures != 0 {
		t.Errorf("response = %+v", resp)
	}
	for _, r := range resp.Results {
		if !r.OK || r.FailedStep != "" || r.Error != "" || r.DurationMs < 0 {
			t.Errorf("result = %+v", r)
		}
	}
	if n := counterValue(t, p.metrics.probes, cujCheckout, resultSuccess, sourceOnDemand); n != 3 {
		t.Errorf("on-demand successes = %v, want 3", n)
	}
	if n := counterValue(t, p.metrics.probes, cujCheckout, resultSuccess, sourceScheduled); n != 0 {
		t.Errorf("scheduled successes = %v, want 0: on-demand runs must not touch SLO series", n)
	}
	if !strings.Contains(logs.String(), `"event":"on_demand_run_completed","cuj":"checkout","runs":3,"successes":3,"failures":0`) {
		t.Errorf("log lacks the summary line:\n%s", logs)
	}
	// Each worker is one shopper: three runs at up to 4 at a time make at most three sessions,
	// and a second call reuses the worker slots instead of adding carts to redis-cart.
	sessions := func() map[string]bool {
		s := map[string]bool{}
		for _, r := range shop.recorded() {
			s[r.session] = true
		}
		return s
	}
	// (Which worker takes which run is up to the scheduler, so only the upper bounds are fixed.)
	if first := sessions(); len(first) > 3 {
		t.Errorf("%d sessions for 3 runs, want at most 3", len(first))
	}
	if rec := postRun(t, t.Context(), p, `{"cuj":"checkout","runs":7}`); rec.Code != http.StatusOK {
		t.Fatalf("second call = %d %s", rec.Code, rec.Body)
	}
	if all := sessions(); len(all) > 4 {
		t.Errorf("%d sessions after 10 runs with 4 workers, want at most 4", len(all))
	}
	if n := shop.cartKeys(); n > 4 {
		t.Errorf("redis-cart would hold %d carts, want at most 4", n)
	}
}

func TestRunReportsFailures(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("POST /cart/checkout", fault{status: 500, body: "failed to charge card"})
	p, _ := testProber(t, shop, func(c *config) { c.runConcurrency = 1 })

	rec := postRun(t, t.Context(), p, `{"cuj":"checkout","runs":2}`)

	var resp runResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s, err %v", rec.Code, rec.Body, err)
	}
	if resp.Successes != 0 || resp.Failures != 2 {
		t.Errorf("response = %+v", resp)
	}
	r := resp.Results[1]
	if r.OK || r.FailedStep != "place-order" || !strings.Contains(r.Error, "failed to charge card") {
		t.Errorf("result = %+v", r)
	}
	if n := counterValue(t, p.metrics.steps, cujCheckout, "place-order", resultFailure, sourceOnDemand); n != 2 {
		t.Errorf("place-order failures = %v, want 2", n)
	}
}

func TestRunDefaultsToOneRun(t *testing.T) {
	p, _ := testProber(t, newFakeShop(t))
	rec := postRun(t, t.Context(), p, `{"cuj":"browse"}`)
	var resp runResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Runs != 1 || len(resp.Results) != 1 {
		t.Errorf("status %d, response %s", rec.Code, rec.Body)
	}
}

func TestRunRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name, body, reason, wantErr string
	}{
		{"not JSON", `cuj=browse`, reasonInvalid, "body must be JSON"},
		{"unknown field", `{"cuj":"browse","runs":2,"parallel":true}`, reasonInvalid, `unknown field "parallel"`},
		{"two objects", `{"cuj":"browse"}{"cuj":"checkout"}`, reasonInvalid, "exactly one JSON object"},
		{"runs as text", `{"cuj":"browse","runs":"20"}`, reasonInvalid, "body must be JSON"},
		{"zero runs", `{"cuj":"browse","runs":0}`, reasonInvalid, "runs must be from 1 to 100"},
		{"too many runs", `{"cuj":"browse","runs":101}`, reasonInvalid, "runs must be from 1 to 100"},
		{"negative runs", `{"cuj":"browse","runs":-5}`, reasonInvalid, "runs must be from 1 to 100"},
		{"unknown journey", `{"cuj":"search","runs":1}`, reasonUnknownCUJ, `unknown cuj "search"; use one of browse, add-to-cart, view-cart, checkout`},
		{"no journey", `{"runs":1}`, reasonUnknownCUJ, `unknown cuj ""`},
		{"body too large", `{"cuj":"` + strings.Repeat("x", 2000) + `"}`, reasonInvalid, "request body too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shop := newFakeShop(t)
			p, _ := testProber(t, shop)
			rec := postRun(t, t.Context(), p, tt.body)
			var e apiError
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("body %q is not an error object: %v", rec.Body, err)
			}
			if rec.Code != http.StatusBadRequest || e.Reason != tt.reason || !strings.Contains(e.Error, tt.wantErr) {
				t.Errorf("got %d %+v, want 400 %s containing %q", rec.Code, e, tt.reason, tt.wantErr)
			}
			if len(shop.routes()) != 0 {
				t.Errorf("a rejected request reached the shop: %v", shop.routes())
			}
		})
	}
}

func TestRunOneAtATimePerJourney(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /product", fault{delay: 300 * time.Millisecond})
	p, _ := testProber(t, shop)

	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- postRun(t, t.Context(), p, `{"cuj":"browse","runs":1}`) }()
	waitFor(t, "the first run to reach the product page", func() bool {
		return slices.Contains(shop.routes(), "GET /product")
	})

	busy := postRun(t, t.Context(), p, `{"cuj":"browse","runs":1}`)
	var e apiError
	_ = json.Unmarshal(busy.Body.Bytes(), &e)
	if busy.Code != http.StatusConflict || e.Reason != reasonBusy {
		t.Errorf("second call = %d %+v, want 409 busy", busy.Code, e)
	}
	// Another journey is not blocked.
	if other := postRun(t, t.Context(), p, `{"cuj":"add-to-cart","runs":1}`); other.Code != http.StatusOK {
		t.Errorf("add-to-cart during a browse run = %d %s", other.Code, other.Body)
	}
	if rec := <-first; rec.Code != http.StatusOK {
		t.Errorf("first call = %d %s", rec.Code, rec.Body)
	}
	if again := postRun(t, t.Context(), p, `{"cuj":"browse","runs":1}`); again.Code != http.StatusOK {
		t.Errorf("browse after the first run ended = %d %s", again.Code, again.Body)
	}
}

func TestRunConcurrencyLimit(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /", fault{delay: 30 * time.Millisecond})
	p, _ := testProber(t, shop, func(c *config) { c.runConcurrency = 3 })

	rec := postRun(t, t.Context(), p, `{"cuj":"browse","runs":12}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if n := shop.maxInflight.Load(); n < 2 || n > 3 {
		t.Errorf("up to %d runs at once, want 2 to 3 with a limit of 3", n)
	}
}

func TestRunCancelled(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /", fault{delay: 10 * time.Second})
	p, _ := testProber(t, shop, func(c *config) { c.stepTimeout = 10 * time.Second })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- postRun(t, ctx, p, `{"cuj":"browse","runs":20}`) }()
	waitFor(t, "the first run", func() bool { return len(shop.routes()) > 0 })

	cancel()
	rec := <-done

	var e apiError
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusServiceUnavailable || e.Reason != reasonCancelled {
		t.Errorf("got %d %+v, want 503 cancelled", rec.Code, e)
	}
	if n := counterValue(t, p.metrics.probes, cujBrowse, resultFailure, sourceOnDemand); n != 0 {
		t.Errorf("cancelled runs counted as %v failures", n)
	}
	if got := len(shop.routes()); got > 4 {
		t.Errorf("%d requests reached the shop; runs kept starting after the cancel", got)
	}
}

func TestRunWhileStopping(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /", fault{delay: 10 * time.Second})
	p, _ := testProber(t, shop, func(c *config) { c.stepTimeout = 10 * time.Second })

	// A run in progress when the prober stops.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- postRun(t, ctx, p, `{"cuj":"browse","runs":1}`) }()
	waitFor(t, "the run", func() bool { return len(shop.routes()) > 0 })
	p.stopping.Store(true)
	cancel()
	var e apiError
	rec := <-done
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusServiceUnavailable || e.Reason != reasonShuttingDown {
		t.Errorf("in-flight run = %d %+v, want 503 shutting-down", rec.Code, e)
	}

	// A new call once stopping.
	rec = postRun(t, t.Context(), p, `{"cuj":"browse"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusServiceUnavailable || e.Reason != reasonShuttingDown {
		t.Errorf("new run = %d %+v, want 503 shutting-down", rec.Code, e)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	p, _ := testProber(t, newFakeShop(t))
	h := p.routes(prometheus.NewRegistry())
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz before start = %d", rec.Code)
	}
	p.ready.Store(true)
	if rec := get("/readyz"); rec.Code != http.StatusOK || rec.Body.String() != "ready\n" {
		t.Errorf("readyz = %d %q", rec.Code, rec.Body)
	}
	if rec := get("/healthz"); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body)
	}
	p.stopping.Store(true)
	if rec := get("/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz while stopping = %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/run", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Errorf("GET /v1/run = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := get("/nothing"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d", rec.Code)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
