package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newTestCluster(env *fakeEnv) *cluster {
	c := &cluster{kube: env.kube, dyn: env.dyn, runID: "testrun", log: discardLogger()}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	c.poll = time.Millisecond
	c.podsGoneTimeout = 50 * time.Millisecond
	return c
}

func TestPreflightPasses(t *testing.T) {
	// The base's own VirtualServices: frontend-public is bound to a gateway,
	// frontend routes another host. Neither blocks a fault.
	env := newFakeEnv(t, nil,
		virtualServiceObj("frontend-public", shopNamespace, nil, []string{"mamori.codewithmuh.com"}, []string{"frontend-public"}),
		virtualServiceObj("frontend", shopNamespace, nil, []string{"frontend.boutique.svc.cluster.local"}, nil),
	)
	if err := newTestCluster(env).preflight(context.Background(), allTargets); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightProblems(t *testing.T) {
	tcpCurrency := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "currencyservice", Namespace: shopNamespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "tcp", Port: 7000}}},
	}
	var typed []runtime.Object
	for _, o := range shopObjects(0) { // redis-cart already at 0
		if s, ok := o.(*corev1.Service); ok && (s.Name == "currencyservice" || s.Name == "emailservice") {
			continue // currency replaced below, email missing
		}
		typed = append(typed, o)
	}
	typed = append(typed, tcpCurrency)
	env := newFakeEnv(t, typed,
		virtualServiceObj("cart-routes", "other", nil, []string{"cartservice.boutique"}, []string{"mesh"}),
		virtualServiceObj("ad-routes", "boutique", nil, []string{"adservice"}, nil),
	)
	err := newTestCluster(env).preflight(context.Background(), allTargets)
	if err == nil {
		t.Fatal("preflight passed")
	}
	for _, s := range []string{
		"currencyservice: no Service port is named grpc",
		"emailservice: get Service",
		"redis-cart: the Deployment already has 0 replicas",
		"cartservice: VirtualService other/cart-routes already routes cartservice.boutique.svc.cluster.local",
		"adservice: VirtualService boutique/ad-routes already routes adservice.boutique.svc.cluster.local",
	} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error lacks %q:\n%v", s, err)
		}
	}
}

func TestPreflightRedisMissing(t *testing.T) {
	env := newFakeEnv(t, []runtime.Object{})
	err := newTestCluster(env).preflight(context.Background(), []target{{"redis-cart", methodScaleToZero}, {"x", "bogus"}})
	if err == nil || !strings.Contains(err.Error(), "get Deployment") || !strings.Contains(err.Error(), `unknown fault method "bogus"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestPreflightFallsBackToShopNamespace(t *testing.T) {
	env := newFakeEnv(t, nil)
	env.dyn.PrependReactor("list", "virtualservices", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.istio.io", Resource: "virtualservices"}, "", errors.New("no"))
		}
		return false, nil, nil
	})
	if err := newTestCluster(env).preflight(context.Background(), allTargets); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightListErrors(t *testing.T) {
	t.Run("own kinds are required", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		env.dyn.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
		err := newTestCluster(env).preflight(context.Background(), allTargets)
		if err == nil || !strings.Contains(err.Error(), "check for existing faults in configmaps: boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("other kinds only warn", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		env.dyn.PrependReactor("list", "podchaos", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("chaos API down")
		})
		if err := newTestCluster(env).preflight(context.Background(), allTargets); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("discovery fails", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		env.kube.Discovery().(*fakediscovery.FakeDiscovery).PrependReactor("get", "group", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("discovery down")
		})
		err := newTestCluster(env).preflight(context.Background(), allTargets)
		if err == nil || !strings.Contains(err.Error(), "discover API resources") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("all-namespace VirtualService list fails", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		env.dyn.PrependReactor("list", "virtualservices", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetNamespace() == "" {
				return true, nil, errors.New("timeout")
			}
			return false, nil, nil
		})
		err := newTestCluster(env).preflight(context.Background(), allTargets)
		if err == nil || !strings.Contains(err.Error(), "list VirtualServices: timeout") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestInjectAndRemoveVirtualService(t *testing.T) {
	env := newFakeEnv(t, nil)
	c := newTestCluster(env)
	f, err := c.inject(context.Background(), target{"currencyservice", methodGRPCAbort})
	if err != nil {
		t.Fatal(err)
	}
	vs, err := env.dyn.Resource(virtualServiceGVR).Namespace(shopNamespace).Get(context.Background(), "cuj-discovery-currencyservice", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	labels := vs.GetLabels()
	if labels[labelFault] != "true" || labels[labelManagedBy] != managedByValue || labels[labelRun] != "testrun" {
		t.Errorf("labels = %v", labels)
	}
	status, _, _ := unstructured.NestedString(vs.Object, "spec", "http", "0", "fault", "abort", "grpcStatus")
	if http, _, _ := unstructured.NestedSlice(vs.Object, "spec", "http"); len(http) != 1 {
		t.Fatalf("http routes = %v", http)
	}
	route := vs.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)
	abort := route["fault"].(map[string]any)["abort"].(map[string]any)
	// The fake may hand the number back as float64 or int64.
	if abort["grpcStatus"] != "UNAVAILABLE" || fmt.Sprint(abort["percentage"].(map[string]any)["value"]) != "100" {
		t.Errorf("abort = %v (%s)", abort, status)
	}
	if err := c.verify(context.Background(), f); err != nil {
		t.Errorf("verify: %v", err)
	}

	// A second inject for the same host must not adopt the existing object.
	if f2, err := c.inject(context.Background(), target{"currencyservice", methodGRPCAbort}); err == nil || f2 != nil {
		t.Errorf("second inject: %v, %v", f2, err)
	}

	if err := c.remove(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err := c.remove(context.Background(), f); err != nil {
		t.Errorf("remove is not idempotent: %v", err)
	}
	if err := c.verify(context.Background(), f); err == nil {
		t.Error("verify passed after removal")
	}
}

func TestVerifyReplacedVirtualService(t *testing.T) {
	env := newFakeEnv(t, nil, virtualServiceObj(objectName("adservice"), shopNamespace, faultLabels("other-run", "adservice"), []string{"adservice"}, nil))
	err := newTestCluster(env).verify(context.Background(), &activeFault{target: target{"adservice", methodGRPCAbort}, name: objectName("adservice")})
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("err = %v", err)
	}
}

func TestScaleToZero(t *testing.T) {
	env := newFakeEnv(t, shopObjects(2))
	c := newTestCluster(env)
	f, err := c.inject(context.Background(), target{"redis-cart", methodScaleToZero})
	if err != nil {
		t.Fatal(err)
	}
	if f.replicas != 2 || redisReplicas(t, env) != 0 {
		t.Fatalf("fault %+v, replicas now %d", f, redisReplicas(t, env))
	}
	cm, err := env.dyn.Resource(configMapGVR).Namespace(shopNamespace).Get(context.Background(), "cuj-discovery-redis-cart", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if data, _, _ := unstructured.NestedStringMap(cm.Object, "data"); data["replicas"] != "2" || data["deployment"] != "redis-cart" {
		t.Errorf("marker data = %v", data)
	}
	if err := c.verify(context.Background(), f); err != nil {
		t.Error(err)
	}
	if err := c.remove(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if n := redisReplicas(t, env); n != 2 {
		t.Errorf("replicas = %d after remove", n)
	}
	if left := faultObjects(t, env); len(left) != 0 {
		t.Errorf("marker left: %v", left)
	}
}

func TestScaleToZeroFailures(t *testing.T) {
	redis := target{"redis-cart", methodScaleToZero}

	t.Run("no deployment", func(t *testing.T) {
		env := newFakeEnv(t, []runtime.Object{})
		if f, err := newTestCluster(env).inject(context.Background(), redis); f != nil || err == nil {
			t.Fatalf("%v, %v", f, err)
		}
	})
	t.Run("marker exists", func(t *testing.T) {
		env := newFakeEnv(t, nil, markerConfigMap("redis-cart", "old", 1))
		f, err := newTestCluster(env).inject(context.Background(), redis)
		if f != nil || err == nil || !strings.Contains(err.Error(), "create marker ConfigMap") {
			t.Fatalf("%v, %v", f, err)
		}
		if redisReplicas(t, env) != 1 {
			t.Error("scaled although the marker could not be written")
		}
	})
	t.Run("patch fails", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		env.kube.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("conflict")
		})
		c := newTestCluster(env)
		f, err := c.inject(context.Background(), redis)
		if f == nil || err == nil || !strings.Contains(err.Error(), "scale Deployment redis-cart to 0: conflict") {
			t.Fatalf("%v, %v", f, err)
		}
		if err := c.remove(context.Background(), f); err == nil {
			t.Error("remove should report the patch failure")
		}
	})
	t.Run("pods never leave", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "redis-cart-abc", Namespace: shopNamespace, Labels: map[string]string{"app": "redis-cart"}}}
		env := newFakeEnv(t, append(shopObjects(1), pod))
		f, err := newTestCluster(env).inject(context.Background(), redis)
		if f == nil || err == nil || !strings.Contains(err.Error(), "pods still running after scaling to 0 (pods left: 1)") {
			t.Fatalf("%v, %v", f, err)
		}
	})
	t.Run("bad selector", func(t *testing.T) {
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "redis-cart", Namespace: shopNamespace},
			Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: "Bogus"},
			}}},
		}
		env := newFakeEnv(t, []runtime.Object{d})
		f, err := newTestCluster(env).inject(context.Background(), redis)
		if f == nil || err == nil || !strings.Contains(err.Error(), "selector") {
			t.Fatalf("%v, %v", f, err)
		}
	})
}

func TestUnknownMethod(t *testing.T) {
	env := newFakeEnv(t, nil)
	c := newTestCluster(env)
	bad := target{"adservice", "bogus"}
	if _, err := c.inject(context.Background(), bad); err == nil {
		t.Error("inject accepted an unknown method")
	}
	if err := c.verify(context.Background(), &activeFault{target: bad}); err == nil {
		t.Error("verify accepted an unknown method")
	}
	if err := c.remove(context.Background(), &activeFault{target: bad}); err == nil {
		t.Error("remove accepted an unknown method")
	}
}

func TestSweep(t *testing.T) {
	t.Run("refuses a broad selector", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		if _, err := newTestCluster(env).sweep(context.Background(), anyFaultSelector, false); err == nil {
			t.Fatal("sweep accepted a selector that matches game-day faults")
		}
	})
	t.Run("bad markers are reported, not acted on", func(t *testing.T) {
		evil := markerConfigMap("redis-cart", "r", 1)
		evil.SetName("cuj-discovery-evil")
		_ = unstructured.SetNestedField(evil.Object, "frontend", "data", "deployment")
		badCount := markerConfigMap("redis-cart", "r", 1)
		badCount.SetName("cuj-discovery-count")
		_ = unstructured.SetNestedField(badCount.Object, "1000", "data", "replicas")
		env := newFakeEnv(t, shopObjects(0), evil, badCount)
		_, err := newTestCluster(env).sweep(context.Background(), ourSelector, false)
		if err == nil || !strings.Contains(err.Error(), `names "frontend"`) || !strings.Contains(err.Error(), `replicas="1000"`) {
			t.Fatalf("err = %v", err)
		}
		if redisReplicas(t, env) != 0 {
			t.Error("acted on an invalid marker")
		}
	})
	t.Run("deployment gone", func(t *testing.T) {
		env := newFakeEnv(t, []runtime.Object{}, markerConfigMap("redis-cart", "r", 1))
		removed, err := newTestCluster(env).sweep(context.Background(), ourSelector, false)
		if err != nil || len(removed) != 1 || len(faultObjects(t, env)) != 0 {
			t.Fatalf("removed %v, err %v", removed, err)
		}
	})
	t.Run("already restored by hand", func(t *testing.T) {
		env := newFakeEnv(t, shopObjects(3), markerConfigMap("redis-cart", "r", 1))
		if _, err := newTestCluster(env).sweep(context.Background(), ourSelector, false); err != nil {
			t.Fatal(err)
		}
		if n := redisReplicas(t, env); n != 3 {
			t.Errorf("replicas = %d, want the hand-set 3 kept", n)
		}
	})
	t.Run("errors keep going", func(t *testing.T) {
		env := newFakeEnv(t, shopObjects(0), markerConfigMap("redis-cart", "r", 1),
			virtualServiceObj(objectName("adservice"), shopNamespace, faultLabels("r", "adservice"), []string{"adservice"}, nil))
		env.kube.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api slow")
		})
		env.dyn.PrependReactor("delete", "virtualservices", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("webhook down")
		})
		removed, err := newTestCluster(env).sweep(context.Background(), ourSelector, false)
		if err == nil || !strings.Contains(err.Error(), "api slow") || !strings.Contains(err.Error(), "webhook down") || len(removed) != 0 {
			t.Fatalf("removed %v, err %v", removed, err)
		}
	})
	t.Run("marker list fails", func(t *testing.T) {
		env := newFakeEnv(t, nil)
		env.dyn.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("nope")
		})
		if _, err := newTestCluster(env).sweep(context.Background(), ourSelector, false); err == nil || !strings.Contains(err.Error(), "list marker ConfigMaps") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestHostMatches(t *testing.T) {
	const fqdn = "cartservice.boutique.svc.cluster.local"
	cases := []struct {
		host, ns string
		want     bool
	}{
		{"cartservice", "boutique", true},
		{"cartservice", "other", false},
		{"cartservice.boutique", "other", true},
		{"cartservice.boutique.svc.cluster.local", "x", true},
		{"CartService.Boutique.svc.cluster.local", "x", true},
		{"*", "x", true},
		{"*.boutique.svc.cluster.local", "x", true},
		{"*.other.svc.cluster.local", "x", false},
		{"currencyservice", "boutique", false},
		{"mamori.codewithmuh.com", "boutique", false},
	}
	for _, c := range cases {
		if got := hostMatches(c.host, c.ns, fqdn); got != c.want {
			t.Errorf("hostMatches(%q, %q) = %v, want %v", c.host, c.ns, got, c.want)
		}
	}
}

func TestSpeaksHTTP(t *testing.T) {
	h2c := "kubernetes.io/h2c"
	tcp := "tcp"
	cases := []struct {
		port corev1.ServicePort
		want bool
	}{
		{corev1.ServicePort{Name: "grpc"}, true},
		{corev1.ServicePort{Name: "http-web"}, true},
		{corev1.ServicePort{Name: "tcp-redis"}, false},
		{corev1.ServicePort{Name: "", AppProtocol: &h2c}, true},
		{corev1.ServicePort{Name: "x", AppProtocol: &tcp}, false},
	}
	for _, c := range cases {
		svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{c.port}}}
		if got := speaksHTTP(svc); got != c.want {
			t.Errorf("speaksHTTP(%+v) = %v", c.port, got)
		}
	}
}

func TestReplicasOf(t *testing.T) {
	two := int32(2)
	if replicasOf(nil) != 1 || replicasOf(&two) != 2 {
		t.Error("replicasOf")
	}
}
