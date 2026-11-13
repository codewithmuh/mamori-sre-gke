package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	streamhttp "k8s.io/streaming/pkg/httpstream"
)

// The prober's Service from the contract (#16).
const (
	proberNamespace = "prober"
	proberService   = "prober"
	proberPort      = 8080
)

// connectProber uses --prober-url when given; otherwise it opens a
// port-forward to the prober Service, like `kubectl port-forward svc/prober`.
// stop closes the port-forward.
func connectProber(ctx context.Context, kc *kubeClients, proberURL string, log *slog.Logger) (prober, func(), error) {
	if proberURL != "" {
		return newHTTPProber(proberURL), func() {}, nil
	}
	local, stop, err := portForwardService(ctx, kc, proberNamespace, proberService, proberPort, log)
	if err != nil {
		return nil, nil, fmt.Errorf("port-forward to %s/%s:%d (or pass --prober-url): %w", proberNamespace, proberService, proberPort, err)
	}
	return newHTTPProber(local), stop, nil
}

func portForwardService(ctx context.Context, kc *kubeClients, ns, svc string, port int32, log *slog.Logger) (string, func(), error) {
	pod, podPort, err := pickPod(ctx, kc.kube, ns, svc, port)
	if err != nil {
		return "", nil, err
	}
	req := kc.kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward")
	dialer, err := newPortForwardDialer(kc.config, req.URL())
	if err != nil {
		return "", nil, err
	}
	// The forward lives until stop is called, not until ctx ends: after Ctrl-C
	// the faults are removed first and the forward closes last.
	fwCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ready := make(chan struct{})
	fw, err := portforward.NewOnAddressesForStreamingWithContext(fwCtx, dialer, []string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", podPort)}, ready, io.Discard, logWriter{log: log})
	if err != nil {
		cancel()
		return "", nil, err
	}
	done := make(chan error, 1)
	go func() { done <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		if err == nil {
			err = errors.New("port-forward ended before it was ready")
		}
		return "", nil, err
	case <-ctx.Done():
		cancel()
		return "", nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err == nil && len(ports) == 0 {
		err = errors.New("no ports")
	}
	if err != nil {
		cancel()
		return "", nil, fmt.Errorf("port-forward has no local port: %w", err)
	}
	local := fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
	log.InfoContext(ctx, "port-forward to the prober ready", "pod", ns+"/"+pod, "local", local)
	return local, cancel, nil
}

// newPortForwardDialer tries WebSockets first and falls back to SPDY, as
// kubectl does.
func newPortForwardDialer(cfg *rest.Config, u *url.URL) (streamhttp.Dialer, error) {
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, err
	}
	spdyDialer := spdy.NewDialerForStreaming(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	wsDialer, err := portforward.NewSPDYOverWebsocketDialerForStreaming(u, cfg)
	if err != nil {
		return nil, err
	}
	return portforward.NewFallbackDialerForStreaming(wsDialer, spdyDialer, func(err error) bool {
		return streamhttp.IsUpgradeFailure(err) || streamhttp.IsHTTPSProxyError(err)
	}), nil
}

// pickPod finds a ready pod behind the Service and the pod port that the
// Service port maps to.
func pickPod(ctx context.Context, kube kubernetes.Interface, ns, name string, port int32) (string, int32, error) {
	svc, err := kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("get Service %s/%s: %w", ns, name, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("service %s/%s has no selector", ns, name)
	}
	i := slices.IndexFunc(svc.Spec.Ports, func(p corev1.ServicePort) bool { return p.Port == port })
	if i < 0 {
		return "", 0, fmt.Errorf("service %s/%s has no port %d", ns, name, port)
	}
	sp := svc.Spec.Ports[i]
	pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return "", 0, fmt.Errorf("list pods of %s/%s: %w", ns, name, err)
	}
	var ready []corev1.Pod
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && podReady(p) {
			ready = append(ready, p)
		}
	}
	if len(ready) == 0 {
		return "", 0, fmt.Errorf("no ready pod behind %s/%s", ns, name)
	}
	slices.SortFunc(ready, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	pod := ready[0]
	podPort, err := targetPort(sp, pod)
	if err != nil {
		return "", 0, fmt.Errorf("pod %s/%s: %w", ns, pod.Name, err)
	}
	return pod.Name, podPort, nil
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func targetPort(sp corev1.ServicePort, pod corev1.Pod) (int32, error) {
	switch {
	case sp.TargetPort.Type == intstr.Int && sp.TargetPort.IntVal == 0:
		return sp.Port, nil // unset: same as the Service port
	case sp.TargetPort.Type == intstr.Int:
		return sp.TargetPort.IntVal, nil
	}
	for _, c := range pod.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == sp.TargetPort.StrVal {
				return cp.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("no container port named %q", sp.TargetPort.StrVal)
}

// logWriter turns the port-forward's error output into log lines.
type logWriter struct{ log *slog.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	if msg := strings.TrimSpace(string(p)); msg != "" {
		w.log.Warn("port-forward", "detail", msg)
	}
	return len(p), nil
}
