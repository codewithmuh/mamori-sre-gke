package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

var (
	podsGVR     = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podChaosGVR = schema.GroupVersionResource{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "podchaos"}
)

// fakeEnv is an in-memory shop: typed objects (Services, Deployments, Pods)
// in a fake clientset, Istio objects and ConfigMaps in a fake dynamic client,
// and a discovery list that includes a Chaos Mesh kind.
type fakeEnv struct {
	kube *kubefake.Clientset
	dyn  *dynamicfake.FakeDynamicClient
	kc   *kubeClients
}

func newFakeEnv(t *testing.T, typed []runtime.Object, dyn ...runtime.Object) *fakeEnv {
	t.Helper()
	if typed == nil {
		typed = shopObjects(1)
	}
	kube := kubefake.NewClientset(typed...)
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Namespaced: true, Kind: "ConfigMap", Verbs: []string{"list", "get"}},
			{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: []string{"list", "get"}},
			{Name: "pods/log", Namespaced: true, Kind: "Pod", Verbs: []string{"get"}},
			{Name: "namespaces", Namespaced: false, Kind: "Namespace", Verbs: []string{"list"}},
			{Name: "bindings", Namespaced: true, Kind: "Binding", Verbs: []string{"create"}},
		}},
		{GroupVersion: "networking.istio.io/v1", APIResources: []metav1.APIResource{
			{Name: "virtualservices", Namespaced: true, Kind: "VirtualService", Verbs: []string{"list", "get", "create", "delete"}},
		}},
		{GroupVersion: "chaos-mesh.org/v1alpha1", APIResources: []metav1.APIResource{
			{Name: "podchaos", Namespaced: true, Kind: "PodChaos", Verbs: []string{"list", "get"}},
		}},
	}
	scheme := runtime.NewScheme()
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		virtualServiceGVR: "VirtualServiceList",
		configMapGVR:      "ConfigMapList",
		podsGVR:           "PodList",
		podChaosGVR:       "PodChaosList",
	}, dyn...)
	return &fakeEnv{kube: kube, dyn: d, kc: &kubeClients{kube: kube, dyn: d, context: "fake", server: "https://fake"}}
}

// shopObjects returns the shop's Services (gRPC ports named "grpc", like
// upstream) and the redis-cart Deployment with the given replicas.
func shopObjects(redisReplicas int32) []runtime.Object {
	var objs []runtime.Object
	for _, t := range allTargets {
		port := corev1.ServicePort{Name: "grpc", Port: 7000}
		if t.Method == methodScaleToZero {
			port = corev1.ServicePort{Name: "tcp-redis", Port: 6379}
		}
		objs = append(objs, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: t.Service, Namespace: shopNamespace},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{port}, Selector: map[string]string{"app": t.Service}},
		})
	}
	objs = append(objs, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "redis-cart", Namespace: shopNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &redisReplicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "redis-cart"}},
		},
	})
	return objs
}

// virtualServiceObj is a VirtualService as another area (or a game day) might
// have made it.
func virtualServiceObj(name, ns string, labels map[string]string, hosts []string, gateways []string) *unstructured.Unstructured {
	spec := map[string]any{"hosts": toAny(hosts)}
	if gateways != nil {
		spec["gateways"] = toAny(gateways)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.istio.io/v1",
		"kind":       "VirtualService",
		"metadata":   map[string]any{"name": name, "namespace": ns, "labels": stringMap(labels)},
		"spec":       spec,
	}}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// truth is the dependency table the fake prober uses: a journey fails every
// run when any of its critical services is broken. It mirrors what the
// v0.10.7 handlers and the prober's steps suggest (see the example map).
var truth = map[string][]string{
	"browse":      {"cartservice", "currencyservice", "productcatalogservice", "redis-cart"},
	"add-to-cart": {"cartservice", "currencyservice", "productcatalogservice", "redis-cart"},
	"view-cart":   {"cartservice", "currencyservice", "productcatalogservice", "redis-cart", "shippingservice"},
	"checkout":    {"cartservice", "checkoutservice", "currencyservice", "paymentservice", "productcatalogservice", "redis-cart", "shippingservice"},
}

// fakeProber answers like the real prober, from the fake cluster's state:
// a service is broken while our VirtualService for it exists, and redis-cart
// while its Deployment has 0 replicas.
type fakeProber struct {
	mu  sync.Mutex
	env *fakeEnv

	// partial[cuj][service] overrides the failure count while service is broken.
	partial map[string]map[string]int

	// baselineFailures fail this many calls while nothing is broken.
	baselineFailures int

	// hook runs at the start of every call, with the broken services.
	hook  func(ctx context.Context, cuj string, broken []string) error
	calls []string
}

func (f *fakeProber) broken(ctx context.Context) []string {
	var out []string
	vss, err := f.env.dyn.Resource(virtualServiceGVR).Namespace(shopNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, vs := range vss.Items {
			if s, ok := strings.CutPrefix(vs.GetName(), "cuj-discovery-"); ok {
				out = append(out, s)
			}
		}
	}
	d, err := f.env.kube.AppsV1().Deployments(shopNamespace).Get(ctx, "redis-cart", metav1.GetOptions{})
	if err == nil && replicasOf(d.Spec.Replicas) == 0 {
		out = append(out, "redis-cart")
	}
	slices.Sort(out)
	return out
}

func (f *fakeProber) Run(ctx context.Context, cuj string, runs int) (runResult, error) {
	if err := ctx.Err(); err != nil {
		return runResult{}, err
	}
	broken := f.broken(ctx)
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s x%d broken=%s", cuj, runs, strings.Join(broken, "+")))
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, cuj, broken); err != nil {
			return runResult{}, err
		}
	}
	failures := 0
	for _, b := range broken {
		if n, ok := f.partial[cuj][b]; ok {
			failures = max(failures, n)
		} else if slices.Contains(truth[cuj], b) {
			failures = runs
		}
	}
	f.mu.Lock()
	if len(broken) == 0 && f.baselineFailures > 0 {
		f.baselineFailures--
		failures = 1
	}
	f.mu.Unlock()
	res := runResult{CUJ: cuj, Runs: runs, Successes: runs - failures, Failures: failures}
	for i := range runs {
		if i < failures {
			res.Results = append(res.Results, runDetail{OK: false, DurationMs: 12, FailedStep: "home", Error: "HTTP 500"})
		} else {
			res.Results = append(res.Results, runDetail{OK: true, DurationMs: 120})
		}
	}
	return res, nil
}

func (f *fakeProber) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// testDeps wires the fakes into the commands. sleep never waits but honours
// cancellation, so loops still end when their context does.
func testDeps(env *fakeEnv, p prober) deps {
	return deps{
		connectKube: func(string, string) (*kubeClients, error) { return env.kc, nil },
		connectProber: func(context.Context, *kubeClients, string, *slog.Logger) (prober, func(), error) {
			return p, func() {}, nil
		},
		now:   func() time.Time { return time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC) },
		sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		poll:  time.Millisecond,

		cleanupTimeout: time.Second,
	}
}

// runCmd runs the CLI and returns exit code, stdout and stderr.
func runCmd(ctx context.Context, d deps, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := dispatch(ctx, args, &out, &errOut, d)
	return code, out.String(), errOut.String()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// faultObjects lists what is left in the fake shop that carries the fault label.
func faultObjects(t *testing.T, env *fakeEnv) []string {
	t.Helper()
	var out []string
	for _, gvr := range []schema.GroupVersionResource{virtualServiceGVR, configMapGVR} {
		list, err := env.dyn.Resource(gvr).Namespace(shopNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: anyFaultSelector})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range list.Items {
			out = append(out, gvr.Resource+"/"+o.GetName())
		}
	}
	return out
}

func redisReplicas(t *testing.T, env *fakeEnv) int32 {
	t.Helper()
	d, err := env.kube.AppsV1().Deployments(shopNamespace).Get(context.Background(), "redis-cart", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return replicasOf(d.Spec.Replicas)
}
