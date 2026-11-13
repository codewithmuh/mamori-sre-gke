package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	virtualServiceGVR = schema.GroupVersionResource{Group: "networking.istio.io", Version: "v1", Resource: "virtualservices"}
	configMapGVR      = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

// activeFault is a fault that is in place now.
type activeFault struct {
	target target

	// name of the VirtualService (gRPC abort) or of the marker ConfigMap
	// (scale to zero).
	name string

	// replicas the Deployment had before it was scaled to zero.
	replicas int32
}

// faultInjector breaks and repairs one dependency at a time.
type faultInjector interface {
	preflight(ctx context.Context, targets []target) error
	inject(ctx context.Context, t target) (*activeFault, error)
	verify(ctx context.Context, f *activeFault) error
	remove(ctx context.Context, f *activeFault) error
	sweep(ctx context.Context, selector string, dryRun bool) ([]string, error)
}

// cluster is the faultInjector for a real (or fake) Kubernetes cluster. Every
// call is scoped to shopNamespace.
type cluster struct {
	kube  kubernetes.Interface
	dyn   dynamic.Interface
	runID string
	log   *slog.Logger
	sleep func(context.Context, time.Duration) error

	// poll is the wait between checks while redis-cart's pods shut down;
	// podsGoneTimeout bounds that wait.
	poll            time.Duration
	podsGoneTimeout time.Duration

	// expiresAt, when set, is stamped on every VirtualService as
	// mamori.dev/expires-at, for the in-cluster reaper.
	expiresAt time.Time

	// Removing a fault retries with backoff (retryMin doubling up to
	// retryMax) for up to cleanupTimeout, so a short API outage can't leave
	// a fault behind.
	cleanupTimeout time.Duration
	retryMin       time.Duration
	retryMax       time.Duration
}

// preflight refuses to start when any fault is already in place (ours from a
// crashed run, or a game-day fault: two faults at once would make the results
// meaningless), and checks that every target can be broken the way we plan.
func (c *cluster) preflight(ctx context.Context, targets []target) error {
	found, err := c.existingFaults(ctx)
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf("namespace %s already has objects labelled %s: %s. Remove them first "+
			"(cuj-discovery cleanup removes this tool's own; gameday stop --all removes game-day faults)",
			shopNamespace, anyFaultSelector, strings.Join(found, ", "))
	}
	vss, err := c.meshVirtualServices(ctx)
	if err != nil {
		return err
	}
	var problems []error
	for _, t := range targets {
		if err := c.checkTarget(ctx, t, vss); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", t.Service, err))
		}
	}
	return errors.Join(problems...)
}

// existingFaults lists every object in the shop namespace labelled
// mamori.dev/fault=true, whatever its kind: game-day faults may be Istio
// objects, Chaos Mesh objects or anything else.
func (c *cluster) existingFaults(ctx context.Context) ([]string, error) {
	var found []string
	// The kinds this tool creates are always checked, and an error there stops
	// the run: without that check a second fault on the same host could slip in.
	for _, gvr := range []schema.GroupVersionResource{virtualServiceGVR, configMapGVR} {
		names, err := c.listFaults(ctx, gvr)
		if err != nil {
			return nil, fmt.Errorf("check for existing faults in %s: %w", gvr.GroupResource(), err)
		}
		found = append(found, names...)
	}

	lists, err := discovery.ServerPreferredNamespacedResourcesWithContext(ctx, discovery.ToDiscoveryInterfaceWithContext(c.kube.Discovery()))
	if err != nil {
		if len(lists) == 0 {
			return nil, fmt.Errorf("discover API resources: %w", err)
		}
		// Typically an aggregated API that is down (metrics.k8s.io). The rest is still checked.
		c.log.WarnContext(ctx, "some API groups could not be discovered; checking the others", "error", err.Error())
	}
	seen := map[schema.GroupResource]bool{virtualServiceGVR.GroupResource(): true, configMapGVR.GroupResource(): true}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			gr := schema.GroupResource{Group: gv.Group, Resource: r.Name}
			if seen[gr] || strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			seen[gr] = true
			names, err := c.listFaults(ctx, gv.WithResource(r.Name))
			if err != nil {
				c.log.WarnContext(ctx, "could not check this kind for faults", "resource", gr.String(), "error", err.Error())
				continue
			}
			found = append(found, names...)
		}
	}
	slices.Sort(found)
	return found, nil
}

func (c *cluster) listFaults(ctx context.Context, gvr schema.GroupVersionResource) ([]string, error) {
	list, err := c.dyn.Resource(gvr).Namespace(shopNamespace).List(ctx, metav1.ListOptions{LabelSelector: anyFaultSelector})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range list.Items {
		names = append(names, gvr.GroupResource().String()+"/"+o.GetName())
	}
	return names, nil
}

// meshVirtualService is the part of a VirtualService that decides whether it
// routes a host for sidecars.
type meshVirtualService struct {
	name      string
	namespace string
	hosts     []string
}

// meshVirtualServices lists VirtualServices that apply to sidecars ("mesh"),
// in every namespace if allowed, else in the shop namespace only.
func (c *cluster) meshVirtualServices(ctx context.Context) ([]meshVirtualService, error) {
	list, err := c.dyn.Resource(virtualServiceGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		c.log.WarnContext(ctx, "cannot list VirtualServices in every namespace; checking only the shop's", "error", err.Error())
		list, err = c.dyn.Resource(virtualServiceGVR).Namespace(shopNamespace).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("list VirtualServices: %w", err)
	}
	var out []meshVirtualService
	for _, o := range list.Items {
		gateways, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "gateways")
		if len(gateways) > 0 && !slices.Contains(gateways, "mesh") {
			continue // bound to gateways only; sidecars never use it
		}
		hosts, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "hosts")
		out = append(out, meshVirtualService{name: o.GetName(), namespace: o.GetNamespace(), hosts: hosts})
	}
	return out, nil
}

func (c *cluster) checkTarget(ctx context.Context, t target, vss []meshVirtualService) error {
	switch t.Method {
	case methodGRPCAbort:
		svc, err := c.kube.CoreV1().Services(shopNamespace).Get(ctx, t.Service, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get Service: %w", err)
		}
		if !speaksHTTP(svc) {
			return errors.New("no Service port is named grpc*/http* or has appProtocol grpc/http/http2: " +
				"Istio could treat it as plain TCP, and the abort would silently not apply")
		}
		host := serviceHost(t.Service)
		for _, vs := range vss {
			for _, h := range vs.hosts {
				if hostMatches(h, vs.namespace, host) {
					return fmt.Errorf("VirtualService %s/%s already routes %s; Istio's behaviour with two "+
						"VirtualServices for one host is undefined, so the abort might not apply", vs.namespace, vs.name, host)
				}
			}
		}
		return nil
	case methodScaleToZero:
		d, err := c.kube.AppsV1().Deployments(shopNamespace).Get(ctx, t.Service, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get Deployment: %w", err)
		}
		if replicasOf(d.Spec.Replicas) < 1 {
			return errors.New("the Deployment already has 0 replicas")
		}
		return nil
	default:
		return fmt.Errorf("unknown fault method %q", t.Method)
	}
}

// speaksHTTP reports whether Istio will treat the Service's traffic as HTTP
// (gRPC is HTTP/2), which HTTP fault injection needs. Online Boutique names
// its gRPC ports "grpc".
func speaksHTTP(svc *corev1.Service) bool {
	for _, p := range svc.Spec.Ports {
		name := strings.ToLower(p.Name)
		if strings.HasPrefix(name, "grpc") || strings.HasPrefix(name, "http") {
			return true
		}
		if p.AppProtocol != nil {
			switch strings.ToLower(*p.AppProtocol) {
			case "grpc", "grpc-web", "http", "http2", "kubernetes.io/h2c":
				return true
			}
		}
	}
	return false
}

// hostMatches reports whether a VirtualService host (as written in a
// VirtualService in namespace ns) covers the fully qualified host fqdn.
func hostMatches(vsHost, ns, fqdn string) bool {
	h := strings.ToLower(vsHost)
	switch {
	case h == "*":
		return true
	case strings.HasPrefix(h, "*."):
		return strings.HasSuffix(fqdn, h[1:])
	case !strings.Contains(h, "."):
		h += "." + ns + ".svc.cluster.local" // short name, resolved in the VirtualService's namespace
	case strings.Count(h, ".") == 1:
		h += ".svc.cluster.local" // name.namespace
	}
	return h == fqdn
}

// inject breaks one dependency. When it returns a non-nil fault, something was
// changed and the caller must remove it, even if an error is returned too.
func (c *cluster) inject(ctx context.Context, t target) (*activeFault, error) {
	switch t.Method {
	case methodGRPCAbort:
		vs := abortVirtualService(t.Service, c.runID, c.expiresAt)
		_, err := c.dyn.Resource(virtualServiceGVR).Namespace(shopNamespace).Create(ctx, vs, metav1.CreateOptions{FieldManager: managedByValue})
		if err != nil {
			// AlreadyExists is an error too: never adopt an object we didn't create.
			return nil, fmt.Errorf("create VirtualService %s: %w", vs.GetName(), err)
		}
		return &activeFault{target: t, name: vs.GetName()}, nil
	case methodScaleToZero:
		return c.scaleToZero(ctx, t)
	default:
		return nil, fmt.Errorf("unknown fault method %q", t.Method)
	}
}

// scaleToZero records the replica count in a marker ConfigMap first, so a
// crashed run can always be repaired by `cleanup`, then scales to 0 and waits
// until the pods are gone.
func (c *cluster) scaleToZero(ctx context.Context, t target) (*activeFault, error) {
	deps := c.kube.AppsV1().Deployments(shopNamespace)
	d, err := deps.Get(ctx, t.Service, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get Deployment %s: %w", t.Service, err)
	}
	replicas := replicasOf(d.Spec.Replicas)
	marker := markerConfigMap(t.Service, c.runID, replicas)
	if _, err := c.dyn.Resource(configMapGVR).Namespace(shopNamespace).Create(ctx, marker, metav1.CreateOptions{FieldManager: managedByValue}); err != nil {
		return nil, fmt.Errorf("create marker ConfigMap %s: %w", marker.GetName(), err)
	}
	f := &activeFault{target: t, name: marker.GetName(), replicas: replicas}
	if err := c.setReplicas(ctx, t.Service, 0); err != nil {
		return f, err
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return f, fmt.Errorf("selector of Deployment %s: %w", t.Service, err)
	}
	wctx, cancel := context.WithTimeout(ctx, c.podsGoneTimeout)
	defer cancel()
	for {
		pods, err := c.kube.CoreV1().Pods(shopNamespace).List(wctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err == nil && len(pods.Items) == 0 {
			return f, nil
		}
		if serr := c.sleep(wctx, c.poll); serr != nil {
			left := -1
			if pods != nil {
				left = len(pods.Items)
			}
			return f, fmt.Errorf("%s pods still running after scaling to 0 (pods left: %d): %w", t.Service, left, errors.Join(serr, err))
		}
	}
}

func (c *cluster) setReplicas(ctx context.Context, deployment string, n int32) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"replicas": n}})
	if err != nil {
		return err
	}
	_, err = c.kube.AppsV1().Deployments(shopNamespace).Patch(ctx, deployment, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: managedByValue})
	if err != nil {
		return fmt.Errorf("scale Deployment %s to %d: %w", deployment, n, err)
	}
	return nil
}

// verify checks that the fault is still in place after the measurement. If
// someone or something removed it (the executor, a person, Argo CD self-heal
// or an autoscaler restoring replicas), the numbers would be wrong.
func (c *cluster) verify(ctx context.Context, f *activeFault) error {
	switch f.target.Method {
	case methodGRPCAbort:
		vs, err := c.dyn.Resource(virtualServiceGVR).Namespace(shopNamespace).Get(ctx, f.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get VirtualService %s: %w", f.name, err)
		}
		if vs.GetLabels()[labelRun] != c.runID {
			return fmt.Errorf("VirtualService %s was replaced by another object", f.name)
		}
		return nil
	case methodScaleToZero:
		d, err := c.kube.AppsV1().Deployments(shopNamespace).Get(ctx, f.target.Service, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get Deployment %s: %w", f.target.Service, err)
		}
		if n := replicasOf(d.Spec.Replicas); n != 0 {
			return fmt.Errorf("someone scaled Deployment %s back to %d replicas during the measurement", f.target.Service, n)
		}
		return nil
	default:
		return fmt.Errorf("unknown fault method %q", f.target.Method)
	}
}

// remove repairs one dependency, retrying each step until it works or
// cleanupTimeout runs out. Safe to repeat.
func (c *cluster) remove(ctx context.Context, f *activeFault) error {
	switch f.target.Method {
	case methodGRPCAbort:
		return c.retry(ctx, "delete VirtualService "+f.name, func(ctx context.Context) error {
			return c.deleteObject(ctx, virtualServiceGVR, f.name)
		})
	case methodScaleToZero:
		// Restore first, then drop the marker: if this stops half way, the
		// marker is still there for `cleanup`.
		err := c.retry(ctx, fmt.Sprintf("scale Deployment %s back to %d", f.target.Service, f.replicas), func(ctx context.Context) error {
			return c.setReplicas(ctx, f.target.Service, f.replicas)
		})
		if err != nil {
			return err
		}
		return c.retry(ctx, "delete marker ConfigMap "+f.name, func(ctx context.Context) error {
			return c.deleteObject(ctx, configMapGVR, f.name)
		})
	default:
		return fmt.Errorf("unknown fault method %q", f.target.Method)
	}
}

// retry runs a clean-up step until it succeeds, backing off from retryMin to
// retryMax, for at most cleanupTimeout (or until ctx ends). Every failure is
// logged as an error: a fault that can't be removed is the worst outcome this
// tool can have.
func (c *cluster) retry(ctx context.Context, step string, fn func(context.Context) error) error {
	if c.cleanupTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cleanupTimeout)
		defer cancel()
	}
	delay := c.retryMin
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			if attempt > 1 {
				c.log.WarnContext(ctx, "clean-up step worked after retrying", "step", step, "attempts", attempt)
			}
			return nil
		}
		c.log.ErrorContext(ctx, "clean-up step failed; retrying", "step", step, "attempt", attempt,
			"retry_in", delay.String(), "error", err.Error())
		if serr := c.sleep(ctx, delay); serr != nil {
			return fmt.Errorf("%s: still failing after %d attempts: %w", step, attempt, err)
		}
		delay = min(max(2*delay, c.retryMin), c.retryMax)
	}
}

func (c *cluster) deleteObject(ctx context.Context, gvr schema.GroupVersionResource, name string) error {
	err := c.dyn.Resource(gvr).Namespace(shopNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s: %w", gvr.GroupResource(), name, err)
	}
	return nil
}

// sweep removes every fault this tool created that matches selector: it
// restores scaled Deployments from their markers, then deletes the markers and
// the VirtualServices. It keeps going after an error, to remove as much as it
// can, and returns what it removed (or, in a dry run, would remove).
func (c *cluster) sweep(ctx context.Context, selector string, dryRun bool) ([]string, error) {
	if !strings.Contains(selector, labelManagedBy+"="+managedByValue) {
		return nil, fmt.Errorf("refusing to sweep with selector %q: it must select only this tool's objects", selector)
	}
	var removed []string
	var errs []error

	markers, err := c.dyn.Resource(configMapGVR).Namespace(shopNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		errs = append(errs, fmt.Errorf("list marker ConfigMaps: %w", err))
	} else {
		for _, m := range markers.Items {
			f, err := faultFromMarker(m)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			desc := fmt.Sprintf("deployment/%s scaled to 0 (restore to %d replicas) + configmap/%s", f.target.Service, f.replicas, f.name)
			if !dryRun {
				err := c.retry(ctx, "restore "+desc, func(ctx context.Context) error { return c.restoreFromMarker(ctx, f) })
				if err != nil {
					errs = append(errs, err)
					continue
				}
			}
			removed = append(removed, desc)
		}
	}

	vss, err := c.dyn.Resource(virtualServiceGVR).Namespace(shopNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		errs = append(errs, fmt.Errorf("list VirtualServices: %w", err))
	} else {
		for _, vs := range vss.Items {
			if !dryRun {
				err := c.retry(ctx, "delete VirtualService "+vs.GetName(), func(ctx context.Context) error {
					return c.deleteObject(ctx, virtualServiceGVR, vs.GetName())
				})
				if err != nil {
					errs = append(errs, err)
					continue
				}
			}
			removed = append(removed, "virtualservice/"+vs.GetName())
		}
	}
	return removed, errors.Join(errs...)
}

// restoreFromMarker scales the Deployment back unless someone already did.
func (c *cluster) restoreFromMarker(ctx context.Context, f *activeFault) error {
	d, err := c.kube.AppsV1().Deployments(shopNamespace).Get(ctx, f.target.Service, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		c.log.WarnContext(ctx, "Deployment named in a marker no longer exists; deleting the marker only", "deployment", f.target.Service)
	case err != nil:
		return fmt.Errorf("get Deployment %s: %w", f.target.Service, err)
	case replicasOf(d.Spec.Replicas) == 0:
		if err := c.setReplicas(ctx, f.target.Service, f.replicas); err != nil {
			return err
		}
	}
	return c.deleteObject(ctx, configMapGVR, f.name)
}

// faultFromMarker reads a marker ConfigMap back. It only accepts markers for
// a known scale-to-zero target, so a hand-made ConfigMap can't make cleanup
// scale an arbitrary Deployment.
func faultFromMarker(m unstructured.Unstructured) (*activeFault, error) {
	data, _, _ := unstructured.NestedStringMap(m.Object, "data")
	name := data["deployment"]
	i := slices.IndexFunc(allTargets, func(t target) bool { return t.Service == name && t.Method == methodScaleToZero })
	if i < 0 {
		return nil, fmt.Errorf("marker ConfigMap %s names %q, which is not a scale-to-zero target; remove it by hand", m.GetName(), name)
	}
	n, err := strconv.ParseInt(data["replicas"], 10, 32)
	if err != nil || n < 1 || n > 100 {
		return nil, fmt.Errorf("marker ConfigMap %s has replicas=%q, want 1-100; restore %s by hand", m.GetName(), data["replicas"], name)
	}
	return &activeFault{target: allTargets[i], name: m.GetName(), replicas: int32(n)}, nil //nolint:gosec // n is 1-100, checked above
}

func replicasOf(r *int32) int32 {
	if r == nil {
		return 1 // the API default
	}
	return *r
}

// abortVirtualService makes every call to the service fail with gRPC status
// UNAVAILABLE (Istio 1.31 HTTPFaultInjection.Abort.grpcStatus). The callers'
// sidecars apply it before routing, so Istio's default retries don't hide it
// (a VirtualService with a fault turns client-side retries off for its route).
// expiresAt, when set, becomes mamori.dev/expires-at, after which the
// in-cluster reaper (#23) may delete it if this tool could not.
func abortVirtualService(service, runID string, expiresAt time.Time) *unstructured.Unstructured {
	host := serviceHost(service)
	annotations := map[string]any{
		annotationAbout: fmt.Sprintf("cuj-discovery run %s: every request to %s fails with gRPC UNAVAILABLE. "+
			"Remove with: make discovery-cleanup", runID, service),
	}
	if !expiresAt.IsZero() {
		annotations[annotationExpiresAt] = expiresAt.UTC().Format(time.RFC3339)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.istio.io/v1",
		"kind":       "VirtualService",
		"metadata": map[string]any{
			"name":        objectName(service),
			"namespace":   shopNamespace,
			"labels":      stringMap(faultLabels(runID, service)),
			"annotations": annotations,
		},
		"spec": map[string]any{
			"hosts": []any{host},
			"http": []any{map[string]any{
				"fault": map[string]any{"abort": map[string]any{
					"grpcStatus": "UNAVAILABLE",
					"percentage": map[string]any{"value": float64(100)},
				}},
				"route": []any{map[string]any{"destination": map[string]any{"host": host}}},
			}},
		},
	}}
}

// markerConfigMap records what scale-to-zero changed, so that `cleanup` can
// undo it after a crash. It gets no mamori.dev/expires-at on purpose: a reaper
// that deleted it would not scale redis-cart back, and would remove the only
// record of the replica count. Only `cleanup` (or this run) removes it.
func markerConfigMap(deployment, runID string, replicas int32) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      objectName(deployment),
			"namespace": shopNamespace,
			"labels":    stringMap(faultLabels(runID, deployment)),
			"annotations": map[string]any{
				annotationAbout: fmt.Sprintf("cuj-discovery run %s: Deployment %s is scaled to 0 and must go back to %d replicas. "+
					"Restore with: make discovery-cleanup", runID, deployment, replicas),
			},
		},
		"data": map[string]any{
			"deployment": deployment,
			"replicas":   strconv.Itoa(int(replicas)),
		},
	}}
}

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
