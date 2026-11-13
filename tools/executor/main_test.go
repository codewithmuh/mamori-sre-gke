package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadSettings(t *testing.T) {
	s, err := loadSettings(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.namespace != "mamori-agent" || s.httpAddr != ":8080" || s.opsAddr != ":9090" || s.authMode != "tokenreview" ||
		s.authAudience != "executor" || len(s.authAllowed) != 1 || s.authAllowed[0] != agentUser {
		t.Fatalf("defaults = %+v", s)
	}
	s, err = loadSettings(env(map[string]string{
		"AUTH_ALLOWED_USERS": " a , b ,", "EXECUTOR_SLACK_APP_TOKEN": "xapp-1", "EXECUTOR_SLACK_BOT_TOKEN": "xoxb-1", "AUTH_MODE": "none",
	}))
	if err != nil || len(s.authAllowed) != 2 || s.slackBotToken != "xoxb-1" || s.slackAPIURL != "https://slack.com/api/" {
		t.Fatalf("custom = %+v, %v", s, err)
	}

	bad := []map[string]string{
		{"POD_NAMESPACE": "Bad_NS"},
		{"AUTH_MODE": "basic"},
		{"AUTH_ALLOWED_USERS": " , "},
		{"EXECUTOR_SLACK_APP_TOKEN": "xapp-1"},
		{"EXECUTOR_SLACK_APP_TOKEN": "xoxb-1", "EXECUTOR_SLACK_BOT_TOKEN": "xoxb-1"},
		{"EXECUTOR_SLACK_APP_TOKEN": "xapp-1", "EXECUTOR_SLACK_BOT_TOKEN": "xoxp-1"},
		{"EXECUTOR_SLACK_API_URL": "ftp://slack.example/api/"},
		{"EXECUTOR_SLACK_API_URL": "https://slack.example/api"},
	}
	for _, b := range bad {
		if _, err := loadSettings(env(b)); err == nil {
			t.Errorf("%v: want an error", b)
		}
	}
}

func TestRunFailsFastOnBadSettings(t *testing.T) {
	var out logBuf
	if code := run(context.Background(), env(map[string]string{"AUTH_MODE": "basic"}), &out); code != 2 {
		t.Errorf("exit code = %d", code)
	}
	// Outside a cluster and without KUBECONFIG there is no Kubernetes config.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if code := run(context.Background(), env(nil), &out); code != 1 {
		t.Errorf("exit code = %d", code)
	}
	if code := run(context.Background(), env(map[string]string{"KUBECONFIG": "/does/not/exist"}), &out); code != 1 {
		t.Errorf("exit code = %d", code)
	}
	if !strings.Contains(out.buf.String(), "invalid settings") {
		t.Errorf("log = %s", out.buf.String())
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func TestServeStartsAndStops(t *testing.T) {
	h := newHarness(t)
	s := settings{namespace: "mamori-agent", httpAddr: freeAddr(t), opsAddr: freeAddr(t)}
	var ready atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() {
		done <- serve(ctx, h.svc.log, h.svc, &api{svc: h.svc, auth: stubAuth{}, log: h.svc.log}, nil,
			promhttp.HandlerFor(h.reg, promhttp.HandlerOpts{}), s, &ready, &health{})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !ready.Load() || !reachable(s.opsAddr) {
		if time.Now().After(deadline) {
			t.Fatal("never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestServeReportsListenErrors(t *testing.T) {
	h := newHarness(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	s := settings{namespace: "mamori-agent", httpAddr: l.Addr().String(), opsAddr: freeAddr(t)}
	var ready atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code := serve(ctx, h.svc.log, h.svc, &api{svc: h.svc, auth: stubAuth{}, log: h.svc.log}, nil,
		promhttp.HandlerFor(h.reg, promhttp.HandlerOpts{}), s, &ready, &health{}); code != 1 {
		t.Errorf("exit code = %d", code)
	}
}

func reachable(addr string) bool {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
