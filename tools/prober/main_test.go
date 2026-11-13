package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRunRejectsBadConfig(t *testing.T) {
	logs := &syncBuffer{}
	code := run(t.Context(), testEnv(map[string]string{"PROBER_INTERVAL": "never"}), logs)
	if code != 2 {
		t.Errorf("run() = %d, want 2", code)
	}
	if !strings.Contains(logs.String(), `"severity":"ERROR","message":"invalid configuration"`) {
		t.Errorf("log = %s", logs)
	}
}

func TestRunFailsWhenItCannotListen(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	logs := &syncBuffer{}
	code := run(t.Context(), testEnv(map[string]string{"PROBER_LISTEN_ADDR": busy.Addr().String()}), logs)
	if code != 1 || !strings.Contains(logs.String(), "cannot listen") {
		t.Errorf("run() = %d, log %s", code, logs)
	}
}

func TestRunStartsAndStops(t *testing.T) {
	shop := newFakeShop(t)
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int)
	go func() {
		done <- run(ctx, testEnv(map[string]string{
			"PROBER_LISTEN_ADDR": "127.0.0.1:0",
			"PROBER_TARGET_URL":  shop.srv.URL,
			"PROBER_HOST":        testHost,
			"PROBER_CUJS":        "browse",
		}), logs)
	}()

	addrPattern := regexp.MustCompile(`"addr":"([^"]+)"`)
	var addr string
	waitFor(t, "the started line", func() bool {
		m := addrPattern.FindStringSubmatch(logs.String())
		if m != nil {
			addr = m[1]
		}
		return m != nil
	})
	waitFor(t, "a scheduled probe", func() bool { return strings.Contains(logs.String(), `"message":"probe succeeded"`) })
	body := httpGet(t, "http://"+addr+"/metrics")
	if !strings.Contains(body, `mamori_cuj_probe_total{cuj="browse",result="success",source="scheduled"} 1`) {
		t.Errorf("metrics lack the probe:\n%s", body)
	}
	if strings.Contains(body, "go_goroutines") {
		t.Error("metrics include the Go runtime collector")
	}

	cancel()
	if code := <-done; code != 0 {
		t.Errorf("run() = %d, want 0", code)
	}
	if !strings.Contains(logs.String(), "prober stopped") {
		t.Errorf("log = %s", logs)
	}
}

func TestServeOverHTTP(t *testing.T) {
	shop := newFakeShop(t)
	p, _ := testProber(t, shop, func(c *config) {
		c.interval = 50 * time.Millisecond
		c.scheduled = []string{cujAddToCart}
	})
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	p.metrics = newMetrics(reg)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error)
	go func() { served <- p.serve(ctx, ln, reg) }()
	base := "http://" + ln.Addr().String()

	waitFor(t, "readiness", func() bool { return p.ready.Load() })
	if got := httpGet(t, base+"/readyz"); got != "ready\n" {
		t.Errorf("readyz = %q", got)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/run", strings.NewReader(`{"cuj":"view-cart","runs":2}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out runResponse
	err = json.NewDecoder(resp.Body).Decode(&out)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Error(cerr)
	}
	if err != nil || resp.StatusCode != http.StatusOK || out.Successes != 2 {
		t.Errorf("POST /v1/run = %d %+v (%v)", resp.StatusCode, out, err)
	}
	waitFor(t, "a scheduled probe", func() bool {
		return counterValue(t, p.metrics.probes, cujAddToCart, resultSuccess, sourceScheduled) >= 1
	})

	cancel()
	if err := <-served; err != nil {
		t.Errorf("serve() = %v", err)
	}
	if !p.stopping.Load() {
		t.Error("stopping flag not set")
	}
}

func TestServeReportsServerErrors(t *testing.T) {
	p, _ := testProber(t, newFakeShop(t))
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.serve(t.Context(), ln, prometheus.NewRegistry()); err == nil {
		t.Error("serve() on a closed listener = nil, want an error")
	}
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
