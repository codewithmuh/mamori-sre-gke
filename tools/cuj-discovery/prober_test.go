package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testProber(url string) *httpProber {
	p := newHTTPProber(url + "/")
	p.busyWait = time.Millisecond
	p.busyRetries = 2
	return p
}

func TestHTTPProberRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/run" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req struct {
			CUJ  string `json:"cuj"`
			Runs int    `json:"runs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		results := []map[string]any{{"ok": false, "durationMs": 5, "failedStep": "place-order", "error": "status 500"}}
		for range req.Runs - 1 {
			results = append(results, map[string]any{"ok": true, "durationMs": 300})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cuj": req.CUJ, "runs": req.Runs, "successes": req.Runs - 1, "failures": 1, "results": results,
		})
	}))
	defer srv.Close()
	res, err := testProber(srv.URL).Run(context.Background(), "checkout", 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failures != 1 || res.Successes != 2 || res.Results[0].FailedStep != "place-order" {
		t.Errorf("res = %+v", res)
	}
	steps, example := failedSteps(res)
	if steps["place-order"] != 1 || example != "status 500" {
		t.Errorf("steps %v example %q", steps, example)
	}
}

func TestHTTPProberBusyThenOK(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"an on-demand run for browse is in progress","reason":"busy"}`))
			return
		}
		_, _ = w.Write([]byte(`{"cuj":"browse","runs":2,"successes":2,"failures":0,"results":[{"ok":true},{"ok":true}]}`))
	}))
	defer srv.Close()
	res, err := testProber(srv.URL).Run(context.Background(), "browse", 2)
	if err != nil || res.Successes != 2 || calls.Load() != 2 {
		t.Fatalf("res %+v err %v calls %d", res, err, calls.Load())
	}
}

func TestHTTPProberErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		runs   int
		want   string
	}{
		{"busy forever", 409, `{"error":"busy","reason":"busy"}`, 1, `HTTP 409 reason="busy"`},
		{"invalid request", 400, `{"error":"runs must be 1-100","reason":"invalid-request"}`, 1, "runs must be 1-100"},
		{"plain text error", 502, "bad gateway", 1, "HTTP 502"},
		{"not JSON", 200, "<html>", 1, "invalid JSON"},
		{"wrong journey", 200, `{"cuj":"browse","runs":1,"successes":1,"failures":0}`, 1, `cuj is "browse", asked for "checkout"`},
		{"wrong count", 200, `{"cuj":"checkout","runs":2,"successes":2,"failures":0}`, 1, "runs is 2, asked for 1"},
		{"sums differ", 200, `{"cuj":"checkout","runs":1,"successes":1,"failures":1}`, 1, "successes 1 + failures 1 != runs 1"},
		{"results differ", 200, `{"cuj":"checkout","runs":2,"successes":2,"failures":0,"results":[{"ok":false},{"ok":true}]}`, 2, "results list (2 entries, 1 failed)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			_, err := testProber(srv.URL).Run(context.Background(), "checkout", c.runs)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestHTTPProberLimitsAndTransport(t *testing.T) {
	p := testProber("http://127.0.0.1:1")
	if _, err := p.Run(context.Background(), "browse", 0); err == nil {
		t.Error("runs=0 accepted")
	}
	if _, err := p.Run(context.Background(), "browse", 101); err == nil {
		t.Error("runs=101 accepted")
	}
	if _, err := p.Run(context.Background(), "browse", 1); err == nil {
		t.Error("no error from a closed port")
	}
	bad := testProber("http://bad host")
	if _, err := bad.Run(context.Background(), "browse", 1); err == nil {
		t.Error("no error from an invalid URL")
	}

	// Cancelled while waiting out a busy prober.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"busy"}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	busy := testProber(srv.URL)
	busy.sleep = sleepCtx
	busy.busyWait = time.Hour
	if _, err := busy.Run(ctx, "browse", 1); err == nil {
		t.Error("expected an error")
	}
}

func TestRateLimitedAndTruncate(t *testing.T) {
	res := runResult{Results: []runDetail{
		{OK: false, Error: "GET /: status 429, want 200 (gateway rate limit): local_rate_limited"},
		{OK: false, Error: "status 500"},
		{OK: true},
	}}
	if n := rateLimited(res); n != 1 {
		t.Errorf("rateLimited = %d", n)
	}
	steps, _ := failedSteps(res)
	if steps["unknown"] != 2 {
		t.Errorf("steps = %v", steps)
	}
	if got := truncate("abcdef", 3); got != "abc..." {
		t.Errorf("truncate = %q", got)
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Error("sleep ignored cancellation")
	}
}
