package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type stubAuth struct{ err error }

func (s stubAuth) Authenticate(context.Context, string) (string, error) { return agentUser, s.err }

func newAPI(t *testing.T, h *harness, auth authenticator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer((&api{svc: h.svc, auth: auth, log: h.svc.log}).routes())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/v1/proposals", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer agent-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const validBody = `{"incidentId":"inc-7","investigationId":"inv-7",
 "action":{"type":"scale","namespace":"boutique","target":"deployment/cartservice","params":{"replicas":2}},
 "rationale":"cartservice is saturated"}`

func TestAPIAcceptsAndReturnsProposal(t *testing.T) {
	h := newHarness(t)
	srv := newAPI(t, h, stubAuth{})
	code, body := post(t, srv.URL, validBody)
	if code != http.StatusAccepted || body["status"] != "pending" || body["proposalId"] == "" {
		t.Fatalf("POST = %d %v", code, body)
	}
	if len(body) != 2 {
		t.Errorf("202 body has extra fields: %v", body)
	}
	id := body["proposalId"].(string)

	code, got := get(t, srv.URL+"/v1/proposals/"+id)
	if code != http.StatusOK || got["status"] != "pending" || got["incidentId"] != "inc-7" {
		t.Fatalf("GET = %d %v", code, got)
	}
	action := got["action"].(map[string]any)
	if action["target"] != "deployment/cartservice" || action["params"].(map[string]any)["replicas"] != float64(2) {
		t.Errorf("action = %v", action)
	}
	for _, k := range []string{"createdAt", "expiresAt", "investigationId", "rationale"} {
		if _, ok := got[k]; !ok {
			t.Errorf("GET lacks %q", k)
		}
	}

	code, got = get(t, srv.URL+"/v1/proposals/p-nope")
	if code != http.StatusNotFound || got["reason"] != reasonNotFound {
		t.Errorf("unknown id = %d %v", code, got)
	}
}

func TestAPIRefusals(t *testing.T) {
	h := newHarness(t)
	srv := newAPI(t, h, stubAuth{})
	cases := []struct {
		name, body string
		code       int
		reason     string
	}{
		{"not json", `{`, 400, reasonInvalid},
		{"unknown field", `{"incidentId":"i","investigationId":"v","action":{"type":"restart","namespace":"boutique","target":"deployment/frontend"},"force":true}`, 400, reasonInvalid},
		{"two values", validBody + `{}`, 400, reasonInvalid},
		{"missing ids", `{"action":{"type":"restart","namespace":"boutique","target":"deployment/frontend"}}`, 400, reasonInvalid},
		{"rollback", `{"incidentId":"i1","investigationId":"v","action":{"type":"rollback","namespace":"boutique","target":"deployment/checkoutservice","params":{"revision":"3"}}}`, 403, reasonNotAllowed},
		{"out of bounds", `{"incidentId":"i2","investigationId":"v","action":{"type":"scale","namespace":"boutique","target":"deployment/cartservice","params":{"replicas":9}}}`, 422, reasonOutOfBounds},
		{"other namespace", `{"incidentId":"i3","investigationId":"v","action":{"type":"restart","namespace":"argocd","target":"deployment/argocd-server"}}`, 403, reasonNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := post(t, srv.URL, c.body)
			if code != c.code || body["reason"] != c.reason || body["error"] == "" {
				t.Fatalf("got %d %v, want %d %s", code, body, c.code, c.reason)
			}
		})
	}

	// already-acted and kill-switch, the other two contract reasons.
	if code, _ := post(t, srv.URL, validBody); code != 202 {
		t.Fatalf("first = %d", code)
	}
	other := strings.Replace(validBody, `"replicas":2`, `"replicas":1`, 1)
	if code, body := post(t, srv.URL, other); code != 409 || body["reason"] != reasonAlreadyActed {
		t.Errorf("second = %d %v", code, body)
	}
	h.cfg.setEnabled(false)
	if code, body := post(t, srv.URL, strings.Replace(validBody, "inc-7", "inc-8", 1)); code != 423 || body["reason"] != reasonKillSwitch {
		t.Errorf("off = %d %v", code, body)
	}

	big := `{"incidentId":"i","investigationId":"v","rationale":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	if code, _ := post(t, srv.URL, big); code != 400 {
		t.Errorf("big body = %d", code)
	}
}

func TestAPIInternalError(t *testing.T) {
	h := newHarness(t)
	h.cluster.checkErr = errors.New("etcd timeout")
	srv := newAPI(t, h, stubAuth{})
	if code, body := post(t, srv.URL, validBody); code != 500 || body["reason"] != reasonInternal {
		t.Errorf("got %d %v", code, body)
	}
}

func TestAPIAuth(t *testing.T) {
	h := newHarness(t)
	srv := newAPI(t, h, stubAuth{err: errUnauthenticated})
	code, body := post(t, srv.URL, validBody)
	if code != 401 || body["reason"] != reasonUnauthorized {
		t.Fatalf("got %d %v", code, body)
	}
	if ev := h.logs.events(t); len(ev) != 1 || ev[0] != "blocked:unauthorized" {
		t.Errorf("events = %v", ev)
	}

	srv = newAPI(t, h, stubAuth{err: errors.New("token review: connection refused")})
	if code, body := post(t, srv.URL, validBody); code != 503 || body["reason"] != reasonInternal {
		t.Fatalf("got %d %v", code, body)
	}

	// GET needs no token: it is read-only and the port is reachable only by the agent.
	if code, _ := get(t, srv.URL+"/v1/proposals/p-nope"); code != 404 {
		t.Errorf("GET = %d", code)
	}
}

func TestAPIMethodNotAllowed(t *testing.T) {
	h := newHarness(t)
	srv := newAPI(t, h, stubAuth{})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/v1/proposals/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d", resp.StatusCode)
	}
}

func TestOpsHandler(t *testing.T) {
	var ready atomic.Bool
	h := newHarness(t)
	live := &health{}
	srv := httptest.NewServer(opsHandler(promhttp.HandlerFor(h.reg, promhttp.HandlerOpts{}), &ready, live))
	defer srv.Close()

	check := func(path string, want int) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, want)
		}
		return string(b)
	}
	check("/healthz", 200)
	check("/readyz", 503)
	ready.Store(true)
	check("/readyz", 200)
	live.fail("Slack Socket Mode stopped: invalid_auth")
	live.fail("a second reason does not replace the first")
	if body := check("/healthz", 503); !strings.Contains(body, "invalid_auth") {
		t.Errorf("healthz body = %q", body)
	}
	h.svc.metrics.event(eventBlocked, "weird-type", "weird-reason")
	body := check("/metrics", 200)
	for _, want := range []string{"mamori_executor_events_total", `action="other"`, `reason="other"`, "mamori_executor_enabled", "mamori_executor_pending_proposals", "mamori_executor_slack_connected"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}
