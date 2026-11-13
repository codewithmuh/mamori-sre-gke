package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestRunEndToEnd(t *testing.T) {
	env := newFakeEnv(t, shopObjects(2))
	// adservice breaks 3 of 20 browse runs: below the threshold, so non-critical,
	// and listed as a partial result.
	p := &fakeProber{env: env, partial: map[string]map[string]int{"browse": {"adservice": 3}}}
	out := filepath.Join(t.TempDir(), "slo", "cuj-map.yaml")

	code, stdout, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--out", out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "cuj-map.golden.yaml")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("map differs from %s (UPDATE_GOLDEN=1 rewrites it)\ngot:\n%s", golden, got)
	}
	m, err := parseCUJMap(got)
	if err != nil {
		t.Fatalf("generated map is invalid: %v", err)
	}
	for _, j := range m.Journeys {
		if !slices.Equal(j.Critical, truth[j.ID]) {
			t.Errorf("%s critical = %v, want %v", j.ID, j.Critical, truth[j.ID])
		}
	}

	if left := faultObjects(t, env); len(left) > 0 {
		t.Errorf("faults left behind: %v", left)
	}
	if n := redisReplicas(t, env); n != 2 {
		t.Errorf("redis-cart replicas = %d, want 2 restored", n)
	}

	calls := p.callLog()
	// 4 baseline calls, then per service 4 measurements + 4 recovery checks.
	if want := 4 + len(allTargets)*8; len(calls) != want {
		t.Errorf("%d prober calls, want %d:\n%s", len(calls), want, strings.Join(calls, "\n"))
	}
	for _, c := range calls[:4] {
		if !strings.HasSuffix(c, "x10 broken=") {
			t.Errorf("baseline call %q: want 10 runs with nothing broken", c)
		}
	}
	// Only one service is ever broken at a time.
	for _, c := range calls {
		if strings.Contains(c, "+") {
			t.Errorf("two faults at once: %s", c)
		}
	}
	for _, s := range []string{"Wrote " + out, "checkout", "browse/adservice=0.15"} {
		if !strings.Contains(stdout, s) {
			t.Errorf("summary lacks %q:\n%s", s, stdout)
		}
	}
	if !strings.Contains(stderr, `"severity":"INFO"`) || !strings.Contains(stderr, `"message":"case measured"`) {
		t.Errorf("expected JSON logs on stderr, got:\n%s", stderr)
	}
}

func TestRunSubset(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env}
	out := filepath.Join(t.TempDir(), "map.yaml")
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--out", out,
		"--cujs", "checkout, browse", "--services", "redis-cart,emailservice", "--runs", "5")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	data, _ := os.ReadFile(out)
	m, err := parseCUJMap(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Journeys) != 2 || m.Journeys[0].ID != "browse" || m.Journeys[1].ID != "checkout" {
		t.Fatalf("journeys = %+v", m.Journeys)
	}
	co := m.Journeys[1]
	if !slices.Equal(co.Critical, []string{"redis-cart"}) || !slices.Equal(co.NonCritical, []string{"emailservice"}) {
		t.Errorf("checkout = %+v", co)
	}
	if m.Metadata.RunsPerCase != 5 {
		t.Errorf("runsPerCase = %d", m.Metadata.RunsPerCase)
	}
}

func TestRunBaselineNotClean(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env, baselineFailures: 1}
	out := filepath.Join(t.TempDir(), "map.yaml")
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--out", out)
	if code != 1 || !strings.Contains(stderr, "baseline is not clean") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, a := range env.dyn.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("created something despite a dirty baseline: %v", a)
		}
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("map written despite the failure: %v", err)
	}
}

func TestRunRefusesWhenAFaultExists(t *testing.T) {
	gameDay := virtualServiceObj("f01-currency-abort", shopNamespace,
		map[string]string{labelFault: "true", "mamori.dev/fault-id": "F01-currency-abort"},
		[]string{"currencyservice"}, nil)
	chaos := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "chaos-mesh.org/v1alpha1",
		"kind":       "PodChaos",
		"metadata":   map[string]any{"name": "f03-redis-cart-pod-kill", "namespace": shopNamespace, "labels": map[string]any{labelFault: "true"}},
	}}
	for name, obj := range map[string]runtime.Object{"virtualservice": gameDay, "chaos mesh": chaos} {
		t.Run(name, func(t *testing.T) {
			env := newFakeEnv(t, nil)
			// Through the tracker with an explicit resource: the fake would
			// guess "podchaoses" as the plural of PodChaos.
			gvr := virtualServiceGVR
			if name == "chaos mesh" {
				gvr = podChaosGVR
			}
			if err := env.dyn.Tracker().Create(gvr, obj, shopNamespace); err != nil {
				t.Fatal(err)
			}
			p := &fakeProber{env: env}
			code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--out", filepath.Join(t.TempDir(), "m.yaml"))
			if code != 1 || !strings.Contains(stderr, "already has objects labelled mamori.dev/fault=true") {
				t.Fatalf("exit %d, stderr:\n%s", code, stderr)
			}
			if len(p.callLog()) != 0 {
				t.Errorf("prober called before preflight passed: %v", p.callLog())
			}
			// The final sweep must not touch someone else's fault.
			if left := faultObjects(t, env); name == "virtualservice" && len(left) != 1 {
				t.Errorf("game-day fault was removed: %v", left)
			}
		})
	}
}

func TestRunRemovesFaultOnProberError(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env}
	p.hook = func(_ context.Context, _ string, broken []string) error {
		if slices.Contains(broken, "currencyservice") {
			return errors.New("prober run browse: HTTP 503")
		}
		return nil
	}
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--out", filepath.Join(t.TempDir(), "m.yaml"))
	if code != 1 || !strings.Contains(stderr, "currencyservice broken") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if left := faultObjects(t, env); len(left) > 0 {
		t.Errorf("faults left behind: %v", left)
	}
}

func TestRunInterruptedRestoresRedis(t *testing.T) {
	env := newFakeEnv(t, shopObjects(3))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &fakeProber{env: env}
	p.hook = func(ctx context.Context, _ string, broken []string) error {
		if slices.Contains(broken, "redis-cart") {
			cancel() // Ctrl-C while redis-cart is scaled to zero
			return ctx.Err()
		}
		return nil
	}
	code, _, stderr := runCmd(ctx, testDeps(env, p), "run", "--out", filepath.Join(t.TempDir(), "m.yaml"))
	if code != 1 || !strings.Contains(stderr, "interrupted; faults removed") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if n := redisReplicas(t, env); n != 3 {
		t.Errorf("redis-cart replicas = %d, want 3", n)
	}
	if left := faultObjects(t, env); len(left) > 0 {
		t.Errorf("faults left behind: %v", left)
	}
}

func TestRunTimeout(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env}
	p.hook = func(ctx context.Context, _ string, broken []string) error {
		if slices.Contains(broken, "adservice") {
			<-ctx.Done() // a call that outlives the whole run
			return ctx.Err()
		}
		return nil
	}
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--timeout", "200ms", "--out", filepath.Join(t.TempDir(), "m.yaml"))
	if code != 1 || !strings.Contains(stderr, "stopped by -timeout 200ms") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if left := faultObjects(t, env); len(left) > 0 {
		t.Errorf("faults left behind: %v", left)
	}
}

func TestRunFaultRemovedByOthers(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env}
	p.hook = func(ctx context.Context, _ string, broken []string) error {
		if slices.Contains(broken, "cartservice") {
			// The executor (or a person) removes the fault mid-measurement.
			return env.dyn.Resource(virtualServiceGVR).Namespace(shopNamespace).Delete(ctx, objectName("cartservice"), metav1.DeleteOptions{})
		}
		return nil
	}
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--out", filepath.Join(t.TempDir(), "m.yaml"))
	if code != 1 || !strings.Contains(stderr, "did not hold for the whole measurement") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestRunRedisScaledBackDuringMeasurement(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env}
	cl := &cluster{kube: env.kube, dyn: env.dyn, log: discardLogger()}
	p.hook = func(ctx context.Context, _ string, broken []string) error {
		if slices.Contains(broken, "redis-cart") {
			return cl.setReplicas(ctx, "redis-cart", 1) // Argo CD self-heal, say
		}
		return nil
	}
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--services", "redis-cart", "--out", filepath.Join(t.TempDir(), "m.yaml"))
	if code != 1 || !strings.Contains(stderr, "redis-cart back to 1 replicas during the measurement") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if left := faultObjects(t, env); len(left) > 0 {
		t.Errorf("marker left behind: %v", left)
	}
}

func TestRunWaitsForRecovery(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env}
	stuck := 0
	p.hook = func(_ context.Context, _ string, broken []string) error {
		if slices.Contains(broken, "redis-cart") {
			stuck = 2 // cartservice needs two more checks to come back
		}
		return nil
	}
	orig := p.hook
	p.hook = func(ctx context.Context, cuj string, broken []string) error {
		if err := orig(ctx, cuj, broken); err != nil {
			return err
		}
		if len(broken) == 0 && stuck > 0 {
			stuck--
			p.baselineFailures = 1
		}
		return nil
	}
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--services", "redis-cart", "--cujs", "browse", "--out", filepath.Join(t.TempDir(), "m.yaml"))
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "journeys have not recovered yet") {
		t.Errorf("expected recovery warnings:\n%s", stderr)
	}
}

func TestRunRecoveryTimeout(t *testing.T) {
	env := newFakeEnv(t, nil)
	p := &fakeProber{env: env}
	measured := false
	p.hook = func(_ context.Context, _ string, broken []string) error {
		if len(broken) > 0 {
			measured = true
		} else if measured { // after the fault: never recovers
			p.baselineFailures = 1
		}
		return nil
	}
	code, _, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--services", "adservice", "--cujs", "browse",
		"--recovery-timeout", "30ms", "--out", filepath.Join(t.TempDir(), "m.yaml"))
	if code != 1 || !strings.Contains(stderr, "journeys still fail 30ms after the fault on adservice was removed") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestRunConnectErrors(t *testing.T) {
	env := newFakeEnv(t, nil)
	d := testDeps(env, &fakeProber{env: env})
	d.connectKube = func(string, string) (*kubeClients, error) { return nil, errors.New("no kubeconfig") }
	if code, _, stderr := runCmd(context.Background(), d, "run"); code != 1 || !strings.Contains(stderr, "no kubeconfig") {
		t.Errorf("exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCmd(context.Background(), d, "cleanup"); code != 1 || !strings.Contains(stderr, "no kubeconfig") {
		t.Errorf("cleanup exit %d: %s", code, stderr)
	}
	d = testDeps(env, nil)
	d.connectProber = func(context.Context, *kubeClients, string, *slog.Logger) (prober, func(), error) {
		return nil, nil, errors.New("no prober pod")
	}
	if code, _, stderr := runCmd(context.Background(), d, "run"); code != 1 || !strings.Contains(stderr, "no prober pod") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

func TestRunWriteError(t *testing.T) {
	env := newFakeEnv(t, nil)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCmd(context.Background(), testDeps(env, &fakeProber{env: env}), "run",
		"--services", "adservice", "--cujs", "browse", "--out", filepath.Join(blocker, "map.yaml"))
	if code != 1 || !strings.Contains(stderr, "write ") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestDryRun(t *testing.T) {
	env := newFakeEnv(t, shopObjects(2))
	p := &fakeProber{env: env}
	code, stdout, stderr := runCmd(context.Background(), testDeps(env, p), "run", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, s := range []string{
		"Dry run 20261120t010203z", "Preflight: passed", "port-forward to service prober/prober:8080",
		"from 2 to 0 replicas", "grpcStatus: UNAVAILABLE", "host: currencyservice.boutique.svc.cluster.local",
		"mamori.dev/fault: \"true\"", "replicas: \"2\"", "kind: ConfigMap",
	} {
		if !strings.Contains(stdout, s) {
			t.Errorf("plan lacks %q:\n%s", s, stdout)
		}
	}
	for _, a := range append(env.dyn.Actions(), env.kube.Actions()...) {
		if v := a.GetVerb(); v != "get" && v != "list" {
			t.Errorf("dry run made a %s call: %v", v, a)
		}
	}
	if len(p.callLog()) != 0 {
		t.Errorf("dry run called the prober")
	}
}

func TestDryRunShowsPreflightFailure(t *testing.T) {
	env := newFakeEnv(t, nil, virtualServiceObj(objectName("adservice"), shopNamespace, faultLabels("old", "adservice"), []string{"adservice"}, nil))
	code, stdout, stderr := runCmd(context.Background(), testDeps(env, nil), "run", "--dry-run", "--prober-url", "http://127.0.0.1:9")
	if code != 1 || !strings.Contains(stdout, "Preflight: FAILED") || !strings.Contains(stderr, "a real run would refuse") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Prober: http://127.0.0.1:9") {
		t.Errorf("plan should name the prober URL:\n%s", stdout)
	}
}

func TestCleanup(t *testing.T) {
	ours := virtualServiceObj(objectName("adservice"), shopNamespace, faultLabels("20261101t000000z", "adservice"), []string{"adservice"}, nil)
	marker := markerConfigMap("redis-cart", "20261101t000000z", 2)
	gameDay := virtualServiceObj("f01-currency-abort", shopNamespace, map[string]string{labelFault: "true"}, []string{"currencyservice"}, nil)

	t.Run("dry run", func(t *testing.T) {
		env := newFakeEnv(t, shopObjects(0), ours.DeepCopy(), marker.DeepCopy(), gameDay.DeepCopy())
		code, stdout, stderr := runCmd(context.Background(), testDeps(env, nil), "cleanup", "--dry-run")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		for _, s := range []string{"would remove virtualservice/cuj-discovery-adservice", "would remove deployment/redis-cart scaled to 0 (restore to 2 replicas)"} {
			if !strings.Contains(stdout, s) {
				t.Errorf("output lacks %q:\n%s", s, stdout)
			}
		}
		if len(faultObjects(t, env)) != 3 || redisReplicas(t, env) != 0 {
			t.Errorf("dry run changed something")
		}
	})

	t.Run("real", func(t *testing.T) {
		env := newFakeEnv(t, shopObjects(0), ours.DeepCopy(), marker.DeepCopy(), gameDay.DeepCopy())
		code, stdout, stderr := runCmd(context.Background(), testDeps(env, nil), "cleanup")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		if n := redisReplicas(t, env); n != 2 {
			t.Errorf("redis-cart replicas = %d, want 2", n)
		}
		if left := faultObjects(t, env); !slices.Equal(left, []string{"virtualservices/f01-currency-abort"}) {
			t.Errorf("left = %v, want only the game-day fault", left)
		}
		if !strings.Contains(stdout, "left alone (not created by cuj-discovery): virtualservices.networking.istio.io/f01-currency-abort") {
			t.Errorf("output:\n%s", stdout)
		}
	})

	t.Run("nothing to do", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		code, stdout, _ := runCmd(context.Background(), testDeps(env, nil), "cleanup")
		if code != 0 || !strings.Contains(stdout, "nothing to remove") {
			t.Errorf("exit %d: %s", code, stdout)
		}
	})

	t.Run("list error", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		env.dyn.PrependReactor("list", "virtualservices", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api down")
		})
		code, _, stderr := runCmd(context.Background(), testDeps(env, nil), "cleanup")
		if code != 1 || !strings.Contains(stderr, "api down") {
			t.Errorf("exit %d: %s", code, stderr)
		}
	})
}

func TestDispatch(t *testing.T) {
	d := testDeps(newFakeEnv(t, nil), nil)
	cases := []struct {
		args []string
		code int
		out  string
	}{
		{nil, 2, "Usage:"},
		{[]string{"version"}, 0, "cuj-discovery 0.1.0"},
		{[]string{"--help"}, 0, "Usage:"},
		{[]string{"frobnicate"}, 2, `unknown command "frobnicate"`},
		{[]string{"run", "-h"}, 0, "-prober-url"},
		{[]string{"run", "extra"}, 2, "unexpected arguments: extra"},
		{[]string{"run", "--runs", "0"}, 2, "-runs=0 must be 1-100"},
		{[]string{"run", "--baseline-runs", "101"}, 2, "-baseline-runs=101"},
		{[]string{"run", "--threshold", "0"}, 2, "-threshold=0"},
		{[]string{"run", "--settle", "-1s"}, 2, "-settle=-1s"},
		{[]string{"run", "--timeout", "0s"}, 2, "must be positive"},
		{[]string{"run", "--out", " "}, 2, "-out must not be empty"},
		{[]string{"run", "--prober-url", "localhost:8080"}, 2, "must be an http(s) URL"},
		{[]string{"run", "--cujs", "search"}, 2, `unknown journey "search"`},
		{[]string{"run", "--services", "frontend"}, 2, `unknown service "frontend"`},
		{[]string{"run", "--bogus"}, 2, "flag provided but not defined"},
		{[]string{"cleanup", "-h"}, 0, "-dry-run"},
		{[]string{"cleanup", "--bogus"}, 2, "flag provided but not defined"},
		{[]string{"cleanup", "extra"}, 2, "unexpected arguments"},
		{[]string{"validate", "-h"}, 0, ""},
		{[]string{"validate", "--bogus"}, 2, ""},
		{[]string{"validate", "a", "b"}, 2, "validate takes one file"},
		{[]string{"validate", "testdata/does-not-exist.yaml"}, 1, "no such file"},
		{[]string{"validate", "testdata/invalid-map.yaml"}, 1, "is not a valid CUJ map"},
		{[]string{"validate", "testdata/cuj-map.golden.yaml"}, 0, "valid (4 journeys, 20 runs per case, threshold 0.5)"},
	}
	for _, c := range cases {
		code, stdout, stderr := runCmd(context.Background(), d, c.args...)
		if code != c.code || !strings.Contains(stdout+stderr, c.out) {
			t.Errorf("%v: exit %d (want %d), output lacks %q:\n%s%s", c.args, code, c.code, c.out, stdout, stderr)
		}
	}
}
