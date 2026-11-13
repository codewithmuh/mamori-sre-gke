package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func proberSvc(target intstr.IntOrString, selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: proberService, Namespace: proberNamespace},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports:    []corev1.ServicePort{{Name: "http", Port: proberPort, TargetPort: target}},
		},
	}
}

func proberPod(name string, ready bool, deleting bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: proberNamespace, Labels: map[string]string{"app": "prober"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "prober", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 9090}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"test"}
	}
	return p
}

func TestPickPod(t *testing.T) {
	sel := map[string]string{"app": "prober"}
	cases := []struct {
		name    string
		objs    []runtime.Object
		pod     string
		port    int32
		wantErr string
	}{
		{"named target port", []runtime.Object{proberSvc(intstr.FromString("http"), sel),
			proberPod("prober-b", true, false), proberPod("prober-a", true, false), proberPod("prober-0", false, false), proberPod("prober-00", true, true)},
			"prober-a", 9090, ""},
		{"numeric target port", []runtime.Object{proberSvc(intstr.FromInt32(8081), sel), proberPod("p", true, false)}, "p", 8081, ""},
		{"unset target port", []runtime.Object{proberSvc(intstr.IntOrString{}, sel), proberPod("p", true, false)}, "p", proberPort, ""},
		{"missing named port", []runtime.Object{proberSvc(intstr.FromString("metrics"), sel), proberPod("p", true, false)}, "", 0, `no container port named "metrics"`},
		{"no service", nil, "", 0, "get Service prober/prober"},
		{"no selector", []runtime.Object{proberSvc(intstr.FromInt32(1), nil)}, "", 0, "has no selector"},
		{"no ready pod", []runtime.Object{proberSvc(intstr.FromInt32(1), sel), proberPod("p", false, false)}, "", 0, "no ready pod"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kube := kubefake.NewClientset(c.objs...)
			pod, port, err := pickPod(context.Background(), kube, proberNamespace, proberService, proberPort)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || pod != c.pod || port != c.port {
				t.Fatalf("got %s:%d, %v; want %s:%d", pod, port, err, c.pod, c.port)
			}
		})
	}
	kube := kubefake.NewClientset(proberSvc(intstr.FromInt32(1), sel))
	if _, _, err := pickPod(context.Background(), kube, proberNamespace, proberService, 1234); err == nil || !strings.Contains(err.Error(), "has no port 1234") {
		t.Errorf("err = %v", err)
	}
}

func TestConnectProberWithURL(t *testing.T) {
	p, stop, err := connectProber(context.Background(), nil, "http://127.0.0.1:8080/", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if hp := p.(*httpProber); hp.baseURL != "http://127.0.0.1:8080" {
		t.Errorf("baseURL = %q", hp.baseURL)
	}
}

// fakeAPIServer serves the prober Service and pod, and refuses the
// port-forward upgrade, so the dial path runs up to the failure.
func fakeAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, obj any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/namespaces/prober/services/prober":
			svc := proberSvc(intstr.FromInt32(8080), map[string]string{"app": "prober"})
			svc.APIVersion, svc.Kind = "v1", "Service"
			write(w, svc)
		case r.URL.Path == "/api/v1/namespaces/prober/pods":
			list := corev1.PodList{Items: []corev1.Pod{*proberPod("prober-1", true, false)}}
			list.APIVersion, list.Kind = "v1", "PodList"
			write(w, list)
		case strings.HasSuffix(r.URL.Path, "/portforward"):
			http.Error(w, "upgrade refused", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPortForwardFailure(t *testing.T) {
	srv := fakeAPIServer(t)
	defer srv.Close()
	cfg := &rest.Config{Host: srv.URL}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	kc := &kubeClients{config: cfg, kube: kube}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err = connectProber(ctx, kc, "", discardLogger())
	if err == nil || !strings.Contains(err.Error(), "port-forward to prober/prober:8080") {
		t.Fatalf("err = %v", err)
	}

	// No pod: fails before dialling.
	_, _, err = portForwardService(ctx, kc, "prober", "missing", 8080, discardLogger())
	if err == nil {
		t.Error("expected an error for a missing Service")
	}

	// Cancelled before the forward is ready.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, _, err := portForwardService(cctx, kc, "prober", "prober", 8080, discardLogger()); err == nil {
		t.Error("expected an error for a cancelled context")
	}
}

func TestLogWriter(t *testing.T) {
	var buf bytes.Buffer
	w := logWriter{log: newLogger(&buf)}
	if n, err := w.Write([]byte("  lost connection to pod \n")); n != 26 || err != nil {
		t.Errorf("Write = %d, %v", n, err)
	}
	_, _ = w.Write([]byte("   "))
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, `"detail":"lost connection to pod"`) || !strings.Contains(got, `"severity":"WARNING"`) {
		t.Errorf("log = %q", got)
	}
}
