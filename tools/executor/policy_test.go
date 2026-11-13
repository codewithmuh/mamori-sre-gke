package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckPolicy(t *testing.T) {
	cfg := testConfig(t, true)
	act := func(typ, target string, params map[string]any) Action {
		return Action{Type: typ, Namespace: targetNamespace, Target: target, Params: params}
	}
	cases := []struct {
		name   string
		a      Action
		reason string // "" = allowed
	}{
		{"restart listed", act(actionRestart, "deployment/currencyservice", nil), ""},
		{"restart unlisted", act(actionRestart, "deployment/redis-cart", nil), reasonNotAllowed},
		{"restart wrong kind", act(actionRestart, "statefulset/currencyservice", nil), reasonNotAllowed},
		{"restart with params", act(actionRestart, "deployment/frontend", map[string]any{"force": true}), reasonInvalid},
		{"scale in bounds", act(actionScale, "deployment/frontend", map[string]any{"replicas": json.Number("3")}), ""},
		{"scale lower bound", act(actionScale, "deployment/cartservice", map[string]any{"replicas": float64(1)}), ""},
		{"scale above", act(actionScale, "deployment/frontend", map[string]any{"replicas": json.Number("4")}), reasonOutOfBounds},
		{"scale to zero", act(actionScale, "deployment/frontend", map[string]any{"replicas": 0}), reasonOutOfBounds},
		{"scale negative", act(actionScale, "deployment/frontend", map[string]any{"replicas": -1}), reasonOutOfBounds},
		{"scale huge", act(actionScale, "deployment/frontend", map[string]any{"replicas": 1e12}), reasonOutOfBounds},
		{"scale fraction", act(actionScale, "deployment/frontend", map[string]any{"replicas": 2.5}), reasonInvalid},
		{"scale string", act(actionScale, "deployment/frontend", map[string]any{"replicas": "2"}), reasonInvalid},
		{"scale bad number", act(actionScale, "deployment/frontend", map[string]any{"replicas": json.Number("2x")}), reasonInvalid},
		{"scale no replicas", act(actionScale, "deployment/frontend", map[string]any{}), reasonInvalid},
		{"scale extra param", act(actionScale, "deployment/frontend", map[string]any{"replicas": 2, "maxSurge": 5}), reasonInvalid},
		{"scale unbounded deployment", act(actionScale, "deployment/currencyservice", map[string]any{"replicas": 2}), reasonNotAllowed},
		{"scale wrong kind", act(actionScale, "hpa/frontend", map[string]any{"replicas": 2}), reasonNotAllowed},
		{"remove-fault vs", act(actionRemoveFault, "virtualservice/f01-currency-abort", nil), ""},
		{"remove-fault chaos", act(actionRemoveFault, "podchaos/f03-redis-cart-pod-kill", nil), ""},
		{"remove-fault kind not configured", act(actionRemoveFault, "sidecar/f04-checkout-bad-config", nil), reasonNotAllowed},
		{"remove-fault unknown kind", act(actionRemoveFault, "secret/anything", nil), reasonNotAllowed},
		{"remove-fault params", act(actionRemoveFault, "virtualservice/x", map[string]any{"a": 1}), reasonInvalid},
		{"rollback", act(actionRollback, "deployment/checkoutservice", map[string]any{"revision": "2"}), reasonNotAllowed},
		{"unknown type", act("exec", "deployment/frontend", nil), reasonNotAllowed},
		{"bad target", act(actionRestart, "frontend", nil), reasonInvalid},
		{"bad target name", act(actionRestart, "deployment/Front_End", nil), reasonInvalid},
		{"nested target", act(actionRestart, "deployment/a/b", nil), reasonInvalid},
		{"other namespace", Action{Type: actionRestart, Namespace: "default", Target: "deployment/frontend"}, reasonNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := checkPolicy(cfg, c.a)
			switch {
			case c.reason == "" && r != nil:
				t.Fatalf("refused: %s %s", r.reason, r.msg)
			case c.reason != "" && r == nil:
				t.Fatalf("allowed, want %s", c.reason)
			case r != nil && r.reason != c.reason:
				t.Fatalf("reason %s (%s), want %s", r.reason, r.msg, c.reason)
			}
		})
	}
}

func TestRollbackExplainsGit(t *testing.T) {
	r := checkPolicy(testConfig(t, true), Action{Type: actionRollback, Namespace: targetNamespace, Target: "deployment/checkoutservice"})
	if r == nil || !strings.Contains(r.msg, "Git") || !strings.Contains(r.msg, "self-heal") {
		t.Fatalf("refusal = %v", r)
	}
}

func TestActionTypeNotInAllowList(t *testing.T) {
	cfg := testConfig(t, true)
	cfg.Restart = nil
	if r := checkPolicy(cfg, Action{Type: actionRestart, Namespace: targetNamespace, Target: "deployment/frontend"}); r == nil || r.reason != reasonNotAllowed {
		t.Fatalf("got %v", r)
	}
}

func TestValidateRequest(t *testing.T) {
	ok := restartRequest("inc-1")
	if r := validateRequest(ok); r != nil {
		t.Fatalf("valid request refused: %v", r)
	}
	mutate := map[string]func(*proposalRequest){
		"no incident":        func(r *proposalRequest) { r.IncidentID = "" },
		"space in incident":  func(r *proposalRequest) { r.IncidentID = "inc 1" },
		"long investigation": func(r *proposalRequest) { r.InvestigationID = strings.Repeat("a", maxIDLen+1) },
		"long rationale":     func(r *proposalRequest) { r.Rationale = strings.Repeat("a", maxRationaleLen+1) },
		"control chars":      func(r *proposalRequest) { r.Rationale = "ok\x1b[31m" },
		"no type":            func(r *proposalRequest) { r.Action.Type = "" },
		"no target":          func(r *proposalRequest) { r.Action.Target = "" },
		"many params": func(r *proposalRequest) {
			r.Action.Params = map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5}
		},
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			req := restartRequest("inc-1")
			m(&req)
			if r := validateRequest(req); r == nil || r.reason != reasonInvalid {
				t.Fatalf("got %v", r)
			}
		})
	}
	// Cloud Monitoring incident ids contain dots; Alertmanager ones are hex.
	for _, id := range []string{"0.nqzj9ah0ksie", "8f3a1c2d", "manual:2026-11-13T09:00:00Z"} {
		req := restartRequest(id)
		if r := validateRequest(req); r != nil {
			t.Errorf("%q refused: %v", id, r)
		}
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Action{
		"scale deployment/frontend in boutique to 2 replicas": {Type: actionScale, Namespace: targetNamespace, Target: "deployment/frontend", Params: map[string]any{"replicas": 2}},
		"restart deployment/frontend in boutique (rolling)":   {Type: actionRestart, Namespace: targetNamespace, Target: "deployment/frontend"},
		"delete fault object virtualservice/x in boutique":    {Type: actionRemoveFault, Namespace: targetNamespace, Target: "virtualservice/x"},
		"rollback deployment/x in boutique":                   {Type: actionRollback, Namespace: targetNamespace, Target: "deployment/x"},
		"scale deployment/x in boutique":                      {Type: actionScale, Namespace: targetNamespace, Target: "deployment/x"},
	}
	for want, a := range cases {
		if got := describe(a); got != want {
			t.Errorf("describe(%v) = %q, want %q", a, got, want)
		}
	}
}

func TestSameAction(t *testing.T) {
	a := Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/x", Params: map[string]any{"replicas": json.Number("2")}}
	b := Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/x", Params: map[string]any{"replicas": float64(2)}}
	c := Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/x", Params: map[string]any{"replicas": 3}}
	if !sameAction(a, b) {
		t.Error("2 and 2.0 should be the same action")
	}
	if sameAction(a, c) {
		t.Error("2 and 3 are different actions")
	}
	if sameAction(a, Action{Params: map[string]any{"x": func() {}}}) {
		t.Error("unencodable params are never equal")
	}
}

func TestHTTPStatus(t *testing.T) {
	want := map[string]int{
		reasonInvalid: 400, reasonUnauthorized: 401, reasonNotAllowed: 403, reasonNotFound: 404,
		reasonAlreadyActed: 409, reasonOutOfBounds: 422, reasonKillSwitch: 423, reasonApprovalUnavailable: 503, "x": 500,
		reasonTooManyOpen: 429, reasonRateLimited: 429,
	}
	for reason, code := range want {
		if got := httpStatus(reason); got != code {
			t.Errorf("httpStatus(%s) = %d, want %d", reason, got, code)
		}
	}
}
