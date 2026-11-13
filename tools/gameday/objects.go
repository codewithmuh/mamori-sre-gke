package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	// faultNamespace is the only namespace gameday ever writes to.
	faultNamespace = "boutique"
	labelFault     = "mamori.dev/fault"
	labelFaultID   = "mamori.dev/fault-id"
	// Set by gameday on every object it applies.
	annotationRunID     = "mamori.dev/run-id"
	annotationExpiresAt = "mamori.dev/expires-at"
)

// faultKinds is the allow-list of object kinds a fault manifest may contain. The resource
// names are fixed here instead of discovered, so a typo in a manifest cannot reach anything
// else. The executor (#25) and the agent (#24) use the same list.
var faultKinds = kindMap([][4]string{
	// group, version, kind, resource
	{"networking.istio.io", "v1", "VirtualService", "virtualservices"},
	{"networking.istio.io", "v1", "Sidecar", "sidecars"},
	{"security.istio.io", "v1", "AuthorizationPolicy", "authorizationpolicies"},
	{"apps", "v1", "Deployment", "deployments"},
	{"chaos-mesh.org", "v1alpha1", "PodChaos", "podchaos"},
	{"chaos-mesh.org", "v1alpha1", "NetworkChaos", "networkchaos"},
	{"chaos-mesh.org", "v1alpha1", "StressChaos", "stresschaos"},
})

func kindMap(rows [][4]string) map[schema.GroupVersionKind]schema.GroupVersionResource {
	m := make(map[schema.GroupVersionKind]schema.GroupVersionResource, len(rows))
	for _, r := range rows {
		m[schema.GroupVersionKind{Group: r[0], Version: r[1], Kind: r[2]}] = schema.GroupVersionResource{Group: r[0], Version: r[1], Resource: r[3]}
	}
	return m
}

// configMapsGVR is read only. cuj-discovery (#17) labels the marker ConfigMap it writes before
// it scales redis-cart to zero as a fault; only `make discovery-cleanup` knows how to restore the
// replicas, so gameday reports such markers and never deletes them.
var configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// discoveryHint is what gameday prints when it finds a discovery marker.
const discoveryHint = "run make discovery-cleanup"

// sortedFaultKinds returns the allow-list in a stable order, for listing.
func sortedFaultKinds() []schema.GroupVersionKind {
	return slices.SortedFunc(maps.Keys(faultKinds), func(a, b schema.GroupVersionKind) int {
		return strings.Compare(a.String(), b.String())
	})
}

// objectRef is "<kind in lowercase>/<name>", the form the executor's targets use.
func objectRef(o *unstructured.Unstructured) string {
	return strings.ToLower(o.GetKind()) + "/" + o.GetName()
}

// parseManifest reads a multi-document YAML file into objects. Empty documents are skipped.
func parseManifest(data []byte) ([]*unstructured.Unstructured, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var objs []*unstructured.Unstructured
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read YAML document: %w", err)
		}
		js, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("YAML document %d: %w", len(objs)+1, err)
		}
		if t := bytes.TrimSpace(js); len(t) == 0 || bytes.Equal(t, []byte("null")) {
			continue
		}
		o := &unstructured.Unstructured{}
		if err := o.UnmarshalJSON(js); err != nil {
			return nil, fmt.Errorf("YAML document %d: %w", len(objs)+1, err)
		}
		objs = append(objs, o)
	}
	if len(objs) == 0 {
		return nil, errors.New("no objects")
	}
	return objs, nil
}

// checkFaultObject enforces the contract for one object of fault id: an allowed kind, in
// boutique, labelled as a fault with this id, named after the fault, and (for Chaos Mesh) with
// selectors that cannot reach outside boutique. Every problem is reported at once.
func checkFaultObject(id string, o *unstructured.Unstructured) error {
	var errs []error
	ref := objectRef(o)
	if _, ok := faultKinds[o.GroupVersionKind()]; !ok {
		errs = append(errs, fmt.Errorf("%s: kind %s is not allowed (allowed: %s)", ref, o.GroupVersionKind(), allowedKindsText()))
	}
	if ns := o.GetNamespace(); ns != faultNamespace {
		errs = append(errs, fmt.Errorf("%s: namespace must be %q, got %q", ref, faultNamespace, ns))
	}
	labels := o.GetLabels()
	if labels[labelFault] != "true" {
		errs = append(errs, fmt.Errorf("%s: label %s must be \"true\"", ref, labelFault))
	}
	if labels[labelFaultID] != id {
		errs = append(errs, fmt.Errorf("%s: label %s must be %q, got %q", ref, labelFaultID, id, labels[labelFaultID]))
	}
	if !strings.HasPrefix(o.GetName(), strings.ToLower(id)) {
		errs = append(errs, fmt.Errorf("%s: name must start with %q", ref, strings.ToLower(id)))
	}
	if o.GroupVersionKind().Group == "chaos-mesh.org" {
		if spec, ok := o.Object["spec"].(map[string]any); ok {
			errs = append(errs, checkSelectorNamespaces(ref, "spec", spec)...)
		}
	}
	return errors.Join(errs...)
}

// checkSelectorNamespaces walks a Chaos Mesh spec. Any "namespaces" list must name only
// boutique, and any "pods" map (namespace -> pod names) may only use boutique as a key.
// Chaos Mesh's namespace-scoped mode refuses the rest too; this catches it before applying.
func checkSelectorNamespaces(ref, path string, v any) []error {
	var errs []error
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			p := path + "." + k
			switch k {
			case "namespaces":
				list, _ := t[k].([]any)
				for _, ns := range list {
					if ns != faultNamespace {
						errs = append(errs, fmt.Errorf("%s: %s may only list %q, got %v", ref, p, faultNamespace, ns))
					}
				}
			case "pods":
				if m, ok := t[k].(map[string]any); ok {
					for ns := range m {
						if ns != faultNamespace {
							errs = append(errs, fmt.Errorf("%s: %s may only use namespace %q, got %q", ref, p, faultNamespace, ns))
						}
					}
				}
			default:
				errs = append(errs, checkSelectorNamespaces(ref, p, t[k])...)
			}
		}
	case []any:
		for i, item := range t {
			errs = append(errs, checkSelectorNamespaces(ref, fmt.Sprintf("%s[%d]", path, i), item)...)
		}
	}
	return errs
}

func allowedKindsText() string {
	var names []string
	for _, gvk := range sortedFaultKinds() {
		names = append(names, gvk.Kind)
	}
	return strings.Join(names, ", ")
}

// meshHosts returns the hosts a VirtualService routes for sidecars (gateway "mesh"), fully
// qualified for namespace boutique. A VirtualService bound only to ingress gateways returns
// none: it cannot clash with a sidecar fault.
func meshHosts(o *unstructured.Unstructured) []string {
	gateways, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "gateways")
	if len(gateways) > 0 && !slices.Contains(gateways, "mesh") {
		return nil
	}
	hosts, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "hosts")
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, qualifyHost(h, o.GetNamespace()))
	}
	return out
}

// qualifyHost expands a short Kubernetes service name the way Istio does for a
// VirtualService in namespace ns.
func qualifyHost(h, ns string) string {
	switch {
	case strings.HasPrefix(h, "*"), strings.HasSuffix(h, ".svc.cluster.local"):
		return h
	case !strings.Contains(h, "."):
		return h + "." + ns + ".svc.cluster.local"
	case strings.Count(h, ".") == 1:
		return h + ".svc.cluster.local"
	case strings.HasSuffix(h, ".svc"):
		return h + ".cluster.local"
	default:
		return h
	}
}

// hostsOverlap reports whether two qualified hosts can match the same request host.
func hostsOverlap(a, b string) bool {
	if a == b || a == "*" || b == "*" {
		return true
	}
	if strings.HasPrefix(a, "*.") && strings.HasSuffix(b, a[1:]) {
		return true
	}
	return strings.HasPrefix(b, "*.") && strings.HasSuffix(a, b[1:])
}

// selectorsOverlap reports whether two Sidecar workloadSelectors could pick the same pod:
// true when one label set contains the other. Istio leaves the result undefined when two
// Sidecars select the same workload.
func selectorsOverlap(a, b map[string]string) bool {
	subset := func(small, big map[string]string) bool {
		for k, v := range small {
			if big[k] != v {
				return false
			}
		}
		return true
	}
	return subset(a, b) || subset(b, a)
}
