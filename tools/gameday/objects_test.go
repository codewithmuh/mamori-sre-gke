package main

import (
	"log/slog"
	"testing"
	"time"
)

func TestQualifyHost(t *testing.T) {
	for in, want := range map[string]string{
		"currencyservice":                            "currencyservice.boutique.svc.cluster.local",
		"currencyservice.boutique":                   "currencyservice.boutique.svc.cluster.local",
		"currencyservice.boutique.svc":               "currencyservice.boutique.svc.cluster.local",
		"currencyservice.boutique.svc.cluster.local": "currencyservice.boutique.svc.cluster.local",
		"*.boutique.svc.cluster.local":               "*.boutique.svc.cluster.local",
		"mamori.codewithmuh.com":                     "mamori.codewithmuh.com",
	} {
		if got := qualifyHost(in, "boutique"); got != want {
			t.Errorf("qualifyHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a.boutique.svc.cluster.local", "a.boutique.svc.cluster.local", true},
		{"a.boutique.svc.cluster.local", "b.boutique.svc.cluster.local", false},
		{"*", "b.boutique.svc.cluster.local", true},
		{"*.boutique.svc.cluster.local", "b.boutique.svc.cluster.local", true},
		{"b.boutique.svc.cluster.local", "*.boutique.svc.cluster.local", true},
		{"*.other.svc.cluster.local", "b.boutique.svc.cluster.local", false},
	}
	for _, tc := range cases {
		if got := hostsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("hostsOverlap(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

func TestSelectorsOverlap(t *testing.T) {
	a := map[string]string{"app": "checkoutservice"}
	if !selectorsOverlap(a, map[string]string{"app": "checkoutservice", "version": "v1"}) {
		t.Error("a subset selector overlaps")
	}
	if selectorsOverlap(a, map[string]string{"app": "frontend"}) {
		t.Error("different apps do not overlap")
	}
}

func TestMeshHosts(t *testing.T) {
	gw := liveObject(t, `apiVersion: networking.istio.io/v1
kind: VirtualService
metadata: {name: frontend-public, namespace: boutique}
spec: {hosts: [mamori.codewithmuh.com], gateways: [frontend-public]}
`)
	if h := meshHosts(gw); len(h) != 0 {
		t.Errorf("a gateway-only VirtualService has no mesh hosts, got %v", h)
	}
	mesh := liveObject(t, `apiVersion: networking.istio.io/v1
kind: VirtualService
metadata: {name: x, namespace: boutique}
spec: {hosts: [cartservice], gateways: [mesh, frontend-public]}
`)
	if h := meshHosts(mesh); len(h) != 1 || h[0] != "cartservice.boutique.svc.cluster.local" {
		t.Errorf("mesh hosts = %v", h)
	}
}

func TestParseManifest(t *testing.T) {
	objs, err := parseManifest([]byte("---\n" + testManifestYAML + "---\n\n---\n" + testManifestYAML))
	if err != nil || len(objs) != 2 {
		t.Fatalf("got %d objects, %v", len(objs), err)
	}
	if objectRef(objs[0]) != "virtualservice/f90-test-abort" {
		t.Errorf("objectRef = %s", objectRef(objs[0]))
	}
	if _, err := parseManifest([]byte("apiVersion: v1\n")); err == nil {
		t.Error("an object without kind should fail")
	}
	if _, err := parseManifest([]byte("a: b: c\n")); err == nil {
		t.Error("invalid YAML should fail")
	}
}

func TestDurationText(t *testing.T) {
	for d, want := range map[string]string{"10m0s": "10m", "90s": "1m30s", "30m0s": "30m"} {
		dur, err := time.ParseDuration(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := durationText(dur); got != want {
			t.Errorf("durationText(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestFaultIDPattern(t *testing.T) {
	for id, want := range map[string]bool{
		"F01-currency-abort": true, "F1-x": false, "f01-x": false, "F01_x": false, "F0a-x": false, "F01-X": false, "F01-": false, "F01-a": true,
	} {
		if got := faultIDPattern(id); got != want {
			t.Errorf("faultIDPattern(%q) = %v", id, got)
		}
	}
}

func TestSeverity(t *testing.T) {
	for lvl, want := range map[int]string{-4: "DEBUG", 0: "INFO", 4: "WARNING", 8: "ERROR"} {
		if got := severity(slog.Level(lvl)); got != want {
			t.Errorf("severity(%d) = %s", lvl, got)
		}
	}
}
