package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLibraryOK(t *testing.T) {
	lib, err := loadLibrary(defaultRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	f, err := lib.get(testFaultID)
	if err != nil {
		t.Fatal(err)
	}
	if f.manual() || f.mode() != "cli" || len(f.objects) != 1 || f.duration.String() != "1m0s" {
		t.Errorf("unexpected fault: manual=%v objects=%d duration=%s", f.manual(), len(f.objects), f.duration)
	}
	m, _ := lib.get("F91-test-manual")
	if !m.manual() || m.mode() != "manual" {
		t.Error("F91 should be manual")
	}
	if _, err := lib.get("F99-nope"); err == nil {
		t.Error("get of an unknown id should fail")
	}
}

// Each case changes the default test repository and expects loadLibrary to fail with a
// message containing want.
func TestLoadLibraryRejects(t *testing.T) {
	replace := func(s, old, repl string) string {
		if !strings.Contains(s, old) {
			panic("test setup: " + old + " not found")
		}
		return strings.Replace(s, old, repl, 1)
	}
	cases := []struct {
		name     string
		fault    string
		manifest string
		extra    map[string]string
		want     string
	}{
		{name: "unknown field", fault: testFaultYAML + "colour: red\n", want: "does not match"},
		{name: "bad category", fault: replace(testFaultYAML, "category: istio-abort", "category: magic"), want: "does not match"},
		{name: "duration over cap", fault: replace(testFaultYAML, "duration: 1m", "duration: 45m"), want: "does not match"},
		{name: "manifest and procedure", fault: testFaultYAML + "procedure: {reason: \"a reason long enough here\", inject: [steps], revert: [steps]}\n", want: "does not match"},
		{name: "none with target", fault: replace(testFaultYAML, "{type: remove-fault, target: virtualservice/f90-test-abort}", "{type: none, target: deployment/x}"), want: "does not match"},
		{name: "slis without cujs", fault: replace(testFaultYAML, "cujs: [browse]", "cujs: []"), want: "does not match"},
		{name: "unsafe action format", fault: replace(testFaultYAML, "unsafeActions: [rollback,", "unsafeActions: [\"drop the database\","), want: "does not match"},
		{name: "remove-fault of another object", fault: replace(testFaultYAML, "target: virtualservice/f90-test-abort}", "target: virtualservice/someone-else}"), want: "is not an object"},
		{name: "rollback of a non-deployment", fault: replace(testFaultYAML, "{type: remove-fault, target: virtualservice/f90-test-abort}", "{type: rollback, target: service/frontend}"), want: "must target deployment/"},
		{name: "wrong namespace", manifest: replace(testManifestYAML, "namespace: boutique", "namespace: default"), want: `namespace must be "boutique"`},
		{name: "no fault label", manifest: replace(testManifestYAML, `mamori.dev/fault: "true"`, `mamori.dev/fault: "false"`), want: "mamori.dev/fault must be"},
		{name: "wrong fault id label", manifest: replace(testManifestYAML, "fault-id: F90-test-abort", "fault-id: F01-currency-abort"), want: "mamori.dev/fault-id must be"},
		{name: "name not from id", manifest: replace(testManifestYAML, "name: f90-test-abort", "name: sneaky"), want: "name must start with"},
		{name: "kind not allowed", manifest: replace(testManifestYAML, "kind: VirtualService", "kind: DestinationRule"), want: "is not allowed"},
		{name: "duplicate object", manifest: testManifestYAML + "---\n" + testManifestYAML, want: "appears twice"},
		{name: "empty manifest", manifest: "---\n# nothing\n", want: "no objects"},
		{name: "broken YAML", manifest: "kind: [unclosed\n", want: "manifest"},
		{name: "file name", extra: map[string]string{"F92-misnamed.yaml": replace(testManualYAML, "F91-test-manual", "F93-other")}, want: "file name must be F93-other.yaml"},
		{name: "same id twice", extra: map[string]string{"F92-dup.yaml": testManualYAML}, want: "file name must be F91-test-manual.yaml"},
		{name: "manual remove-fault", extra: map[string]string{"F94-manual-rf.yaml": replace(replace(testManualYAML, "F91-test-manual", "F94-manual-rf"), "{type: none}", "{type: remove-fault, target: virtualservice/x}")}, want: "manual fault has no objects"},
		{name: "fault YAML broken", extra: map[string]string{"F95-broken.yaml": "id: [\n"}, want: "F95-broken.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				testFaultID + ".yaml":                testFaultYAML,
				"manifests/" + testFaultID + ".yaml": testManifestYAML,
				"F91-test-manual.yaml":               testManualYAML,
			}
			if tc.fault != "" {
				files[testFaultID+".yaml"] = tc.fault
			}
			if tc.manifest != "" {
				files["manifests/"+testFaultID+".yaml"] = tc.manifest
			}
			for k, v := range tc.extra {
				files[k] = v
			}
			_, err := loadLibrary(writeRepo(t, files))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadLibraryChaosSelectors(t *testing.T) {
	manifest := `apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: f90-test-abort
  namespace: boutique
  labels: {mamori.dev/fault: "true", mamori.dev/fault-id: F90-test-abort}
spec:
  action: partition
  mode: all
  selector:
    namespaces: [boutique, kube-system]
    pods: {argocd: [argocd-server-0]}
  target:
    mode: all
    selector: {namespaces: [istio-system]}
`
	fault := strings.Replace(testFaultYAML, "virtualservice/f90-test-abort", "networkchaos/f90-test-abort", 1)
	_, err := loadLibrary(writeRepo(t, map[string]string{
		testFaultID + ".yaml":                fault,
		"manifests/" + testFaultID + ".yaml": manifest,
	}))
	if err == nil {
		t.Fatal("selectors outside boutique must be refused")
	}
	for _, want := range []string{"spec.selector.namespaces", "kube-system", "spec.selector.pods", "argocd", "spec.target.selector.namespaces", "istio-system"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%v", want, err)
		}
	}
}

func TestLoadLibraryMissingPieces(t *testing.T) {
	empty := t.TempDir()
	if _, err := loadLibrary(empty); err == nil || !strings.Contains(err.Error(), "read fault schema") {
		t.Errorf("missing schema: got %v", err)
	}
	noFaults := writeRepo(t, nil)
	if _, err := loadLibrary(noFaults); err == nil || !strings.Contains(err.Error(), "no fault files") {
		t.Errorf("no faults: got %v", err)
	}
	badSchema := writeRepo(t, nil)
	if err := os.WriteFile(filepath.Join(badSchema, schemaPath), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLibrary(badSchema); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("bad schema JSON: got %v", err)
	}
	invalidSchema := writeRepo(t, nil)
	if err := os.WriteFile(filepath.Join(invalidSchema, schemaPath), []byte(`{"type": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLibrary(invalidSchema); err == nil || !strings.Contains(err.Error(), "compile") {
		t.Errorf("invalid schema: got %v", err)
	}
	noManifest := writeRepo(t, map[string]string{testFaultID + ".yaml": testFaultYAML})
	if _, err := loadLibrary(noManifest); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("missing manifest: got %v", err)
	}
}
