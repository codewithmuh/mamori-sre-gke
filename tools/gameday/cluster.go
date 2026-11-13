package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// newKubeClient builds a dynamic client from the usual kubeconfig rules (KUBECONFIG, then
// ~/.kube/config), with optional overrides, and returns the context name it uses.
func newKubeClient(kubeconfig, kubeContext string) (dynamic.Interface, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("read kubeconfig: %w", err)
	}
	name := kubeContext
	if name == "" {
		name = raw.CurrentContext
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig context %q: %w", name, err)
	}
	cfg.UserAgent = "mamori-gameday/" + version
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, "", err
	}
	return dc, name, nil
}

// cluster wraps the dynamic client. Every call is pinned to namespace boutique.
type cluster struct {
	dc dynamic.Interface
}

func (c cluster) resource(o *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvr, ok := faultKinds[o.GroupVersionKind()]
	if !ok {
		return nil, fmt.Errorf("%s: kind %s is not allowed", objectRef(o), o.GroupVersionKind())
	}
	return c.dc.Resource(gvr).Namespace(faultNamespace), nil
}

// activeObjects lists every allowed kind in boutique with the given label selector. A kind
// whose CRD is missing (Chaos Mesh not installed yet) has nothing to list.
func (c cluster) activeObjects(ctx context.Context, selector string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, gvk := range sortedFaultKinds() {
		list, err := c.dc.Resource(faultKinds[gvk]).Namespace(faultNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %s in %s: %w", faultKinds[gvk].Resource, faultNamespace, err)
		}
		for i := range list.Items {
			o := list.Items[i]
			o.SetGroupVersionKind(gvk)
			out = append(out, &o)
		}
	}
	return out, nil
}

// conflicts finds live objects that would make the fault's result undefined: another
// VirtualService for the same host on the mesh (Istio's "one VirtualService per host"), or
// another Sidecar for the same pods. A #17 discovery run, for example, uses such
// VirtualServices.
func (c cluster) conflicts(ctx context.Context, objs []*unstructured.Unstructured) ([]string, error) {
	var out []string
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		if gvk.Kind != "VirtualService" && gvk.Kind != "Sidecar" {
			continue
		}
		list, err := c.dc.Resource(faultKinds[gvk]).Namespace(faultNamespace).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", faultKinds[gvk].Resource, err)
		}
		for i := range list.Items {
			live := &list.Items[i]
			live.SetGroupVersionKind(gvk)
			if live.GetName() == o.GetName() {
				continue
			}
			if why := clash(o, live); why != "" {
				out = append(out, fmt.Sprintf("%s (%s)", objectRef(live), why))
			}
		}
	}
	return out, nil
}

func clash(want, live *unstructured.Unstructured) string {
	if want.GetKind() == "VirtualService" {
		for _, a := range meshHosts(want) {
			for _, b := range meshHosts(live) {
				if hostsOverlap(a, b) {
					return "also routes " + b
				}
			}
		}
		return ""
	}
	a, _, _ := unstructured.NestedStringMap(want.Object, "spec", "workloadSelector", "labels")
	b, _, _ := unstructured.NestedStringMap(live.Object, "spec", "workloadSelector", "labels")
	// A Sidecar without a selector is the namespace default; a workload's own Sidecar wins
	// over it, so they only clash when both are defaults.
	if (len(a) == 0) != (len(b) == 0) {
		return ""
	}
	if selectorsOverlap(a, b) {
		return "selects the same pods"
	}
	return ""
}

func (c cluster) create(ctx context.Context, o *unstructured.Unstructured) error {
	r, err := c.resource(o)
	if err != nil {
		return err
	}
	_, err = r.Create(ctx, o, metav1.CreateOptions{FieldManager: "gameday"})
	if err != nil && apierrors.IsNotFound(err) {
		return fmt.Errorf("create %s: %w (is the CRD installed? Chaos Mesh comes from deploy/apps/chaos-mesh.yaml)", objectRef(o), err)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", objectRef(o), err)
	}
	return nil
}

// state reports whether the object is still there, and whether its deletion has started (a
// Chaos Mesh object waits for its finalizer while Chaos Mesh recovers the pods).
func (c cluster) state(ctx context.Context, o *unstructured.Unstructured) (exists, deleting bool, err error) {
	r, err := c.resource(o)
	if err != nil {
		return false, false, err
	}
	live, err := r.Get(ctx, o.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, live.GetDeletionTimestamp() != nil, nil
}

// exists reports whether the object is still there (deleting counts as there).
func (c cluster) exists(ctx context.Context, o *unstructured.Unstructured) (bool, error) {
	there, _, err := c.state(ctx, o)
	return there, err
}

// remove deletes the objects, then waits until they are gone. Chaos Mesh objects carry a
// finalizer that is only removed once the fault has been recovered in the pods, so "gone"
// means "recovered".
func (c cluster) remove(ctx context.Context, objs []*unstructured.Unstructured, timeout, poll time.Duration) error {
	var errs []error
	for _, o := range objs {
		r, err := c.resource(o)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		bg := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, o.GetName(), metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", objectRef(o), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		var left []string
		for _, o := range objs {
			there, err := c.exists(ctx, o)
			if err != nil || there {
				left = append(left, objectRef(o))
			}
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			slices.Sort(left)
			return fmt.Errorf("still present after %s: %s. A Chaos Mesh object waits for chaos-controller-manager "+
				"to recover its pods; check `kubectl -n chaos-mesh get pods` and the object's status", timeout, strings.Join(left, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
