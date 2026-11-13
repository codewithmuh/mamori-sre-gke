package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// realRepo is the repository root seen from this package (tools/gameday).
const realRepo = "../.."

const testFaultID = "F90-test-abort"

const testFaultYAML = `id: F90-test-abort
title: Test abort fault
category: istio-abort
gameDay: false
manifest: manifests/F90-test-abort.yaml
target: {namespace: boutique, kind: Deployment, name: currencyservice}
duration: 1m
description: A test fault that aborts every currency call.
expected:
  cujs: [browse]
  slis: [availability]
  rootCause: {service: currencyservice, summary: "A test summary that is long enough for the schema."}
  correctAction: {type: remove-fault, target: virtualservice/f90-test-abort}
  unsafeActions: [rollback, "restart deployment/redis-cart"]
`

const testManifestYAML = `apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: f90-test-abort
  namespace: boutique
  labels:
    mamori.dev/fault: "true"
    mamori.dev/fault-id: F90-test-abort
spec:
  hosts: [currencyservice.boutique.svc.cluster.local]
  http:
    - fault: {abort: {grpcStatus: UNAVAILABLE, percentage: {value: 100}}}
      route: [{destination: {host: currencyservice.boutique.svc.cluster.local}}]
`

const testManualYAML = `id: F91-test-manual
title: Test manual fault
category: node
gameDay: false
procedure:
  reason: A node operation that gameday must not run itself.
  inject: ["kubectl cordon NODE"]
  revert: ["kubectl uncordon NODE"]
target: {namespace: boutique, kind: Deployment, name: redis-cart}
duration: 1m
description: A test fault that a person injects by hand.
expected:
  cujs: []
  slis: []
  rootCause: {service: node, summary: "A test summary that is long enough for the schema."}
  correctAction: {type: none}
  unsafeActions: []
`

// writeRepo makes a repository with the real schema and the given files (paths relative to
// chaos/faults).
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	schemaData, err := os.ReadFile(filepath.Join(realRepo, schemaPath))
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, schemaPath), string(schemaData))
	for name, content := range files {
		mustWrite(t, filepath.Join(repo, faultsDir, name), content)
	}
	return repo
}

func defaultRepo(t *testing.T) string {
	t.Helper()
	return writeRepo(t, map[string]string{
		testFaultID + ".yaml":                testFaultYAML,
		"manifests/" + testFaultID + ".yaml": testManifestYAML,
		"F91-test-manual.yaml":               testManualYAML,
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeClient is a dynamic client that knows every allowed fault kind.
func fakeClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{}
	for gvk, gvr := range faultKinds {
		listKinds[gvr] = gvk.Kind + "List"
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
}

// syncBuffer is a bytes.Buffer safe for the race detector.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testApp(repo string, dc dynamic.Interface, stdin string) (*app, *syncBuffer, *syncBuffer) {
	out, errb := &syncBuffer{}, &syncBuffer{}
	a := newApp(out, errb, strings.NewReader(stdin))
	a.getwd = func() (string, error) { return repo, nil }
	a.newClient = func(string, string) (dynamic.Interface, string, error) { return dc, "test-context", nil }
	a.minDuration = time.Millisecond
	a.pollInterval = 5 * time.Millisecond
	a.cleanupTimeout = 300 * time.Millisecond
	return a, out, errb
}

// liveObject builds an unstructured object as the API server would return it.
func liveObject(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	objs, err := parseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return objs[0]
}

// apiNotFound is what the API server answers for a kind whose CRD is not installed.
func apiNotFound() error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "networking.istio.io", Resource: "virtualservices"}, "")
}
