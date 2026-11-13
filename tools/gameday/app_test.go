package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
)

var vsGVR = schema.GroupVersionResource{Group: "networking.istio.io", Version: "v1", Resource: "virtualservices"}

func TestCommandsWithoutCluster(t *testing.T) {
	repo := defaultRepo(t)
	cases := []struct {
		args     []string
		code     int
		inStdout string
	}{
		{args: nil, code: exitRefused},
		{args: []string{"help"}, code: exitOK, inStdout: "Usage:"},
		{args: []string{"version"}, code: exitOK, inStdout: "gameday " + version},
		{args: []string{"bogus"}, code: exitRefused},
		{args: []string{"list"}, code: exitOK, inStdout: "F90-test-abort"},
		{args: []string{"list", "extra"}, code: exitRefused},
		{args: []string{"list", "-o", "yaml"}, code: exitRefused},
		{args: []string{"list", "--nope"}, code: exitRefused},
		{args: []string{"list", "-h"}, code: exitOK},
		{args: []string{"start"}, code: exitRefused},
		{args: []string{"start", "F99-missing"}, code: exitRefused},
		{args: []string{"start", testFaultID, "--duration", "2h"}, code: exitRefused},
		{args: []string{"start", testFaultID, "--manual"}, code: exitRefused},
		{args: []string{"start", "F91-test-manual"}, code: exitManual, inStdout: "kubectl uncordon NODE"},
		{args: []string{"start", "F91-test-manual", "-o", "json"}, code: exitManual, inStdout: `"event":"manual"`},
		{args: []string{"stop"}, code: exitRefused},
		{args: []string{"stop", "not-an-id"}, code: exitRefused},
		{args: []string{"stop", "--all", testFaultID}, code: exitRefused},
		{args: []string{"status", "extra"}, code: exitRefused},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			a, out, errb := testApp(repo, fakeClient(), "")
			if code := a.run(context.Background(), tc.args); code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out, errb)
			}
			if !strings.Contains(out.String(), tc.inStdout) {
				t.Errorf("stdout lacks %q:\n%s", tc.inStdout, out)
			}
		})
	}
}

func TestListJSON(t *testing.T) {
	a, out, _ := testApp(realRepo, fakeClient(), "")
	if code := a.run(context.Background(), []string{"list", "-o", "json"}); code != exitOK {
		t.Fatal(code)
	}
	var faults []faultSummary
	if err := json.Unmarshal([]byte(out.String()), &faults); err != nil {
		t.Fatal(err)
	}
	if len(faults) < 20 || faults[0].ID != "F01-currency-abort" || !faults[0].GameDay || faults[0].Mode != "cli" {
		t.Errorf("unexpected list: %+v", faults[:1])
	}
	if strings.Contains(out.String(), "rootCause") || strings.Contains(out.String(), "remove-fault") {
		t.Error("list must not print the expected answers")
	}
}

func TestStartRunsAndCleansUp(t *testing.T) {
	repo := defaultRepo(t)
	dc := fakeClient()
	a, out, _ := testApp(repo, dc, "")
	code := a.run(context.Background(), []string{"start", testFaultID, "--duration", "60ms", "-o", "json"})
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two JSON lines, got:\n%s", out)
	}
	var started startedLine
	var finished finishedLine
	if err := json.Unmarshal([]byte(lines[0]), &started); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &finished); err != nil {
		t.Fatal(err)
	}
	if started.Event != "started" || started.FaultID != testFaultID || len(started.Objects) != 1 || finished.Reason != reasonDuration {
		t.Errorf("started %+v finished %+v", started, finished)
	}
	// The object is gone afterwards.
	list, err := dc.Resource(vsGVR).Namespace(faultNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(list.Items) != 0 {
		t.Errorf("objects left: %v %v", list, err)
	}
	// The object carried the run's annotations while it was there.
	var created *unstructured.Unstructured
	for _, act := range dc.Actions() {
		if c, ok := act.(k8stesting.CreateAction); ok {
			created, _ = c.GetObject().(*unstructured.Unstructured)
		}
	}
	if created == nil || created.GetAnnotations()[annotationRunID] != started.RunID || created.GetAnnotations()[annotationExpiresAt] == "" {
		t.Errorf("created object annotations: %v", created)
	}
	tl := readTimeline(t, started.Timeline)
	if tl.Status != statusFinished || tl.EndReason != reasonDuration || tl.KubeContext != "test-context" {
		t.Errorf("timeline %+v", tl)
	}
	for _, ev := range []string{"run_started", "object_applied", "fault_active", "removing", "fault_removed", "run_finished"} {
		if !hasEvent(tl, ev) {
			t.Errorf("timeline lacks %s: %+v", ev, tl.Events)
		}
	}
	if !strings.HasPrefix(started.Timeline, filepath.Join(repo, runsDir)) {
		t.Errorf("timeline path %s", started.Timeline)
	}
}

func TestStartTextOutputAndSignal(t *testing.T) {
	repo := defaultRepo(t)
	a, out, _ := testApp(repo, fakeClient(), "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if code := a.run(ctx, []string{"start", testFaultID, "--duration", "10m"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "is active") || !strings.Contains(out.String(), "finished (signal)") {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestStartNoticesExternalRemoval(t *testing.T) {
	repo := defaultRepo(t)
	dc := fakeClient()
	a, out, _ := testApp(repo, dc, "")
	go func() {
		// Act like the executor's remove-fault: delete the object while the run holds it.
		for i := 0; i < 2000; i++ {
			time.Sleep(5 * time.Millisecond)
			if err := dc.Resource(vsGVR).Namespace(faultNamespace).Delete(context.Background(), "f90-test-abort", metav1.DeleteOptions{}); err == nil {
				return
			}
		}
	}()
	if code := a.run(context.Background(), []string{"start", testFaultID, "--duration", "10m", "-o", "json"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), `"reason":"removed-externally"`) {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestStartRefusals(t *testing.T) {
	repo := defaultRepo(t)
	other := liveObject(t, strings.ReplaceAll(testManifestYAML, "f90-test-abort", "f01-currency-abort"))
	other.SetLabels(map[string]string{labelFault: "true", labelFaultID: "F01-currency-abort"})
	discovery := liveObject(t, `apiVersion: networking.istio.io/v1
kind: VirtualService
metadata: {name: discovery-currencyservice, namespace: boutique}
spec: {hosts: [currencyservice]}
`)
	cases := []struct {
		name string
		live runtime.Object
		want string
	}{
		{name: "another fault active", live: other, want: "another fault is active"},
		{name: "VirtualService for the same host", live: discovery, want: "would clash with virtualservice/discovery-currencyservice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, errb := testApp(repo, fakeClient(tc.live), "")
			if code := a.run(context.Background(), []string{"start", testFaultID}); code != exitRefused {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, errb)
			}
		})
	}
}

func TestStartSidecarConflict(t *testing.T) {
	sidecarFault := strings.NewReplacer("category: istio-abort", "category: config", "virtualservice/f90", "sidecar/f90").Replace(testFaultYAML)
	sidecar := `apiVersion: networking.istio.io/v1
kind: Sidecar
metadata:
  name: f90-test-abort
  namespace: boutique
  labels: {mamori.dev/fault: "true", mamori.dev/fault-id: F90-test-abort}
spec:
  workloadSelector: {labels: {app: checkoutservice}}
  outboundTrafficPolicy: {mode: REGISTRY_ONLY}
`
	repo := writeRepo(t, map[string]string{testFaultID + ".yaml": sidecarFault, "manifests/" + testFaultID + ".yaml": sidecar})
	nsDefault := liveObject(t, "apiVersion: networking.istio.io/v1\nkind: Sidecar\nmetadata: {name: default, namespace: boutique}\nspec: {}\n")
	same := liveObject(t, "apiVersion: networking.istio.io/v1\nkind: Sidecar\nmetadata: {name: tuned, namespace: boutique}\nspec: {workloadSelector: {labels: {app: checkoutservice}}}\n")

	a, _, _ := testApp(repo, fakeClient(nsDefault), "")
	if code := a.run(context.Background(), []string{"start", testFaultID, "--duration", "20ms"}); code != exitOK {
		t.Errorf("a namespace-default Sidecar must not block: exit %d", code)
	}
	a, _, errb := testApp(repo, fakeClient(same), "")
	if code := a.run(context.Background(), []string{"start", testFaultID}); code != exitRefused || !strings.Contains(errb.String(), "selects the same pods") {
		t.Errorf("same selector: exit %d\n%s", code, errb)
	}
}

func TestStartApplyFailureRollsBack(t *testing.T) {
	second := strings.ReplaceAll(testManifestYAML, "name: f90-test-abort", "name: f90-test-abort-2")
	repo := writeRepo(t, map[string]string{
		testFaultID + ".yaml":                testFaultYAML,
		"manifests/" + testFaultID + ".yaml": testManifestYAML + "---\n" + second,
	})
	dc := fakeClient()
	dc.PrependReactor("create", "virtualservices", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if obj.GetName() == "f90-test-abort-2" {
			return true, nil, errors.New("admission webhook denied it")
		}
		return false, nil, nil
	})
	a, _, errb := testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"start", testFaultID}); code != exitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "admission webhook denied it") {
		t.Errorf("stderr:\n%s", errb)
	}
	list, _ := dc.Resource(vsGVR).Namespace(faultNamespace).List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Errorf("the first object must be rolled back, left: %d", len(list.Items))
	}
	tl := latestTimeline(t, repo)
	if tl.Status != statusFailed || tl.EndReason != reasonApply {
		t.Errorf("timeline %+v", tl)
	}
}

func TestStartCleanupFailure(t *testing.T) {
	repo := defaultRepo(t)
	dc := fakeClient()
	// A Chaos Mesh finalizer that never clears: delete "succeeds" but the object stays.
	dc.PrependReactor("delete", "virtualservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	a, _, errb := testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"start", testFaultID, "--duration", "20ms"}); code != exitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "still present") || !strings.Contains(errb.String(), "gameday stop "+testFaultID) {
		t.Errorf("stderr:\n%s", errb)
	}
	if tl := latestTimeline(t, repo); tl.EndReason != reasonCleanup {
		t.Errorf("timeline %+v", tl)
	}
}

func TestStartMissingCRD(t *testing.T) {
	repo := defaultRepo(t)
	dc := fakeClient()
	dc.PrependReactor("create", "virtualservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiNotFound()
	})
	a, _, errb := testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"start", testFaultID}); code != exitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "is the CRD installed") {
		t.Errorf("stderr:\n%s", errb)
	}
}

func TestStartClientError(t *testing.T) {
	a, _, errb := testApp(defaultRepo(t), nil, "")
	a.newClient = func(string, string) (dynamic.Interface, string, error) { return nil, "", errors.New("no kubeconfig") }
	for _, args := range [][]string{{"start", testFaultID}, {"stop", "--all"}, {"status"}} {
		if code := a.run(context.Background(), args); code != exitError {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if !strings.Contains(errb.String(), "no kubeconfig") {
		t.Errorf("stderr:\n%s", errb)
	}
}

func TestStartManual(t *testing.T) {
	repo := defaultRepo(t)
	a, out, _ := testApp(repo, nil, "\n\n")
	if code := a.run(context.Background(), []string{"start", "F91-test-manual", "--manual", "--duration", "20ms"}); code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	tl := latestTimeline(t, repo)
	for _, ev := range []string{"fault_injected", "revert_requested", "fault_reverted"} {
		if !hasEvent(tl, ev) {
			t.Errorf("timeline lacks %s: %+v", ev, tl.Events)
		}
	}
	if tl.Mode != "manual" || tl.Status != statusFinished {
		t.Errorf("timeline %+v", tl)
	}

	// No input at all: the run is aborted before injection.
	a, _, _ = testApp(defaultRepo(t), nil, "")
	if code := a.run(context.Background(), []string{"start", "F91-test-manual", "--manual"}); code != exitRefused {
		t.Errorf("aborted manual run: exit %d", code)
	}

	// Ctrl-C during the run, then no confirmation of the revert.
	repo = defaultRepo(t)
	a, _, _ = testApp(repo, nil, "\n")
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel only once the injection is on record, so a slow machine cannot cancel first.
	go func() {
		defer cancel()
		for i := 0; i < 2000; i++ {
			time.Sleep(5 * time.Millisecond)
			files, _ := filepath.Glob(filepath.Join(repo, runsDir, "*.json"))
			for _, f := range files {
				if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), "fault_injected") { // #nosec G304 -- test file
					return
				}
			}
		}
	}()
	if code := a.run(ctx, []string{"start", "F91-test-manual", "--manual", "--duration", "10m"}); code != exitOK {
		t.Errorf("signalled manual run: exit %d", code)
	}
	if tl := latestTimeline(t, repo); tl.EndReason != reasonSignal || !hasEvent(tl, "revert_not_confirmed") {
		t.Errorf("timeline %+v", tl)
	}
}

func TestStopAndStatus(t *testing.T) {
	repo := defaultRepo(t)
	mine := liveObject(t, testManifestYAML)
	mine.SetAnnotations(map[string]string{annotationRunID: "run-1", annotationExpiresAt: "2026-01-01T00:00:00Z"})
	mine.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	notMine := liveObject(t, "apiVersion: networking.istio.io/v1\nkind: VirtualService\nmetadata: {name: frontend, namespace: boutique}\nspec: {hosts: [frontend]}\n")
	dc := fakeClient(mine, notMine)

	a, out, _ := testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"status"}); code != exitOK || !strings.Contains(out.String(), "EXPIRED") {
		t.Fatalf("status exit %d:\n%s", code, out)
	}
	a, out, _ = testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"status", "-o", "json"}); code != exitOK {
		t.Fatal(code)
	}
	var st struct{ Active []activeFault }
	if err := json.Unmarshal([]byte(out.String()), &st); err != nil || len(st.Active) != 1 || !st.Active[0].Expired || st.Active[0].RunID != "run-1" {
		t.Fatalf("status json %s: %v", out, err)
	}

	a, out, _ = testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"stop", "F01-currency-abort"}); code != exitOK || !strings.Contains(out.String(), "nothing to stop") {
		t.Errorf("stop of another id: %d %s", code, out)
	}
	a, out, _ = testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"stop", testFaultID, "-o", "json"}); code != exitOK || !strings.Contains(out.String(), "virtualservice/f90-test-abort") {
		t.Errorf("stop: %d %s", code, out)
	}
	a, out, _ = testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"stop", "--all"}); code != exitOK || !strings.Contains(out.String(), "nothing to stop") {
		t.Errorf("stop --all again: %d %s", code, out)
	}
	// The shop's own VirtualService is untouched.
	if _, err := dc.Resource(vsGVR).Namespace(faultNamespace).Get(context.Background(), "frontend", metav1.GetOptions{}); err != nil {
		t.Errorf("stop removed an object that is not a fault: %v", err)
	}
	a, out, _ = testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"status"}); code != exitOK || !strings.Contains(out.String(), "no fault objects") {
		t.Errorf("status after stop: %s", out)
	}
}

func TestStopText(t *testing.T) {
	dc := fakeClient(liveObject(t, testManifestYAML))
	a, out, _ := testApp(defaultRepo(t), dc, "")
	if code := a.run(context.Background(), []string{"stop", "--all"}); code != exitOK || !strings.Contains(out.String(), "removed virtualservice/f90-test-abort (F90-test-abort)") {
		t.Errorf("stop --all: %d %s", code, out)
	}
}

func TestListErrorsAreReported(t *testing.T) {
	dc := fakeClient()
	dc.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	for _, args := range [][]string{{"status"}, {"stop", "--all"}, {"start", testFaultID}} {
		a, _, errb := testApp(defaultRepo(t), dc, "")
		if code := a.run(context.Background(), args); code != exitError || !strings.Contains(errb.String(), "forbidden") {
			t.Errorf("%v: exit %d\n%s", args, code, errb)
		}
	}
}

func TestRepoRoot(t *testing.T) {
	repo := defaultRepo(t)
	deep := filepath.Join(repo, "tools", "gameday")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	a, _, _ := testApp(deep, nil, "")
	got, err := a.repoRoot("")
	if err != nil || got != repo {
		t.Errorf("repoRoot from %s = %s, %v", deep, got, err)
	}
	if _, err := a.repoRoot(t.TempDir()); err == nil {
		t.Error("--repo without chaos/faults must fail")
	}
	if got, err := a.repoRoot(repo); err != nil || got != repo {
		t.Errorf("--repo: %s %v", got, err)
	}
	a.getwd = func() (string, error) { return t.TempDir(), nil }
	if _, err := a.repoRoot(""); err == nil {
		t.Error("no repo above the working directory must fail")
	}
	a.getwd = func() (string, error) { return "", errors.New("gone") }
	if _, err := a.repoRoot(""); err == nil {
		t.Error("getwd error must fail")
	}
}

func TestNewKubeClient(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	mustWrite(t, kubeconfig, `apiVersion: v1
kind: Config
clusters:
- {name: lab, cluster: {server: "https://127.0.0.1:6443"}}
users:
- {name: me, user: {token: test-token}}
contexts:
- {name: lab, context: {cluster: lab, user: me}}
- {name: other, context: {cluster: lab, user: me}}
current-context: lab
`)
	if _, name, err := newKubeClient(kubeconfig, ""); err != nil || name != "lab" {
		t.Errorf("current context: %s %v", name, err)
	}
	if _, name, err := newKubeClient(kubeconfig, "other"); err != nil || name != "other" {
		t.Errorf("explicit context: %s %v", name, err)
	}
	if _, _, err := newKubeClient(kubeconfig, "missing"); err == nil {
		t.Error("unknown context must fail")
	}
	bad := filepath.Join(t.TempDir(), "bad")
	mustWrite(t, bad, "not: [yaml")
	if _, _, err := newKubeClient(bad, ""); err == nil {
		t.Error("broken kubeconfig must fail")
	}
}

func TestTimelineSaveError(t *testing.T) {
	f := &fault{ID: "F90-test-abort"}
	dir := t.TempDir()
	// A file where the runs directory should be.
	mustWrite(t, filepath.Join(dir, runsDir), "x")
	tl := newTimeline(dir, "run", f, time.Minute, "", time.Now)
	if err := tl.add("x", "", ""); err == nil {
		t.Error("saving under a file must fail")
	}
}

func readTimeline(t *testing.T, path string) timeline {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	var tl timeline
	if err := json.Unmarshal(data, &tl); err != nil {
		t.Fatal(err)
	}
	return tl
}

func latestTimeline(t *testing.T, repo string) timeline {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(repo, runsDir, "*.json"))
	if len(files) == 0 {
		t.Fatal("no timeline written")
	}
	return readTimeline(t, files[len(files)-1])
}

func hasEvent(tl timeline, name string) bool {
	for _, e := range tl.Events {
		if e.Event == name {
			return true
		}
	}
	return false
}

func TestStartRecordsDeletionStart(t *testing.T) {
	repo := defaultRepo(t)
	dc := fakeClient()
	gone := liveObject(t, testManifestYAML)
	now := metav1.Now()
	gone.SetDeletionTimestamp(&now)
	var calls atomic.Int32
	// The executor deletes a Chaos Mesh object: it lingers with a deletion timestamp while
	// Chaos Mesh recovers the pods, then disappears.
	dc.PrependReactor("get", "virtualservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		switch n := calls.Add(1); {
		case n < 3:
			return false, nil, nil
		case n < 6:
			return true, gone.DeepCopy(), nil
		default:
			return true, nil, apiNotFound()
		}
	})
	a, _, _ := testApp(repo, dc, "")
	if code := a.run(context.Background(), []string{"start", testFaultID, "--duration", "10m"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	tl := latestTimeline(t, repo)
	if !hasEvent(tl, "object_deletion_started") || !hasEvent(tl, "object_removed_externally") || tl.EndReason != reasonRemoved {
		t.Errorf("timeline %+v", tl)
	}
}
