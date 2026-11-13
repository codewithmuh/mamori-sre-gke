package main

import (
	"fmt"
	"slices"
	"strings"
)

// shopNamespace is fixed on purpose and has no flag: the tool must never
// create, change or delete anything outside the shop.
const shopNamespace = "boutique"

// allCUJs are the journey ids the prober serves (PRD CUJ-1..4), sorted.
var allCUJs = []string{"add-to-cart", "browse", "checkout", "view-cart"}

// faultMethod is how a dependency is broken.
type faultMethod string

const (
	// methodGRPCAbort: an Istio VirtualService on the service's host whose
	// fault aborts 100 % of requests with gRPC status UNAVAILABLE. Callers'
	// sidecars apply it, so the service itself keeps running.
	methodGRPCAbort faultMethod = "istio-grpc-abort"

	// methodScaleToZero: the Deployment is scaled to 0 replicas and later back.
	// Used for redis-cart, which speaks plain TCP: Istio's HTTP faults don't
	// apply to it.
	methodScaleToZero faultMethod = "scale-to-zero"
)

// target is one upstream dependency and how to break it.
type target struct {
	Service string
	Method  faultMethod
}

// allTargets are the upstream services from the contract, sorted. The
// frontend and the load generator are not targets: breaking the frontend
// breaks every journey by definition.
var allTargets = []target{
	{"adservice", methodGRPCAbort},
	{"cartservice", methodGRPCAbort},
	{"checkoutservice", methodGRPCAbort},
	{"currencyservice", methodGRPCAbort},
	{"emailservice", methodGRPCAbort},
	{"paymentservice", methodGRPCAbort},
	{"productcatalogservice", methodGRPCAbort},
	{"recommendationservice", methodGRPCAbort},
	{"redis-cart", methodScaleToZero},
	{"shippingservice", methodGRPCAbort},
}

// Labels and annotations on everything the tool creates. mamori.dev/fault is
// the label every fault in the lab carries (the executor and the game-day CLI
// look for it); managed-by and the run id tell this tool's faults apart from
// game-day faults.
const (
	labelFault      = "mamori.dev/fault"
	labelManagedBy  = "app.kubernetes.io/managed-by"
	labelPartOf     = "app.kubernetes.io/part-of"
	labelRun        = "mamori.dev/discovery-run"
	labelTarget     = "mamori.dev/discovery-target"
	annotationAbout = "mamori.dev/description"
	managedByValue  = "cuj-discovery"

	// anyFaultSelector matches every fault in the lab, ours or not.
	anyFaultSelector = labelFault + "=true"

	// ourSelector matches only faults this tool created.
	ourSelector = labelFault + "=true," + labelManagedBy + "=" + managedByValue
)

func faultLabels(runID, service string) map[string]string {
	return map[string]string{
		labelFault:     "true",
		labelManagedBy: managedByValue,
		labelPartOf:    "mamori",
		labelRun:       runID,
		labelTarget:    service,
	}
}

// objectName is the name of the VirtualService or marker ConfigMap for a target.
func objectName(service string) string { return "cuj-discovery-" + service }

// serviceHost is the in-mesh host name of a Service in the shop namespace.
func serviceHost(service string) string {
	return service + "." + shopNamespace + ".svc.cluster.local"
}

// selectCUJs returns the journeys named in a comma-separated list, or all of
// them when the list is empty. The result is sorted and has no duplicates.
func selectCUJs(list string) ([]string, error) {
	names := splitList(list)
	if len(names) == 0 {
		return slices.Clone(allCUJs), nil
	}
	for _, n := range names {
		if !slices.Contains(allCUJs, n) {
			return nil, fmt.Errorf("unknown journey %q (known: %s)", n, strings.Join(allCUJs, ", "))
		}
	}
	return names, nil
}

// selectTargets returns the services named in a comma-separated list, or all
// of them when the list is empty, in the order of allTargets.
func selectTargets(list string) ([]target, error) {
	names := splitList(list)
	if len(names) == 0 {
		return slices.Clone(allTargets), nil
	}
	var out []target
	for _, n := range names {
		i := slices.IndexFunc(allTargets, func(t target) bool { return t.Service == n })
		if i < 0 {
			var known []string
			for _, t := range allTargets {
				known = append(known, t.Service)
			}
			return nil, fmt.Errorf("unknown service %q (known: %s)", n, strings.Join(known, ", "))
		}
		out = append(out, allTargets[i])
	}
	slices.SortFunc(out, func(a, b target) int { return strings.Compare(a.Service, b.Service) })
	return out, nil
}

// splitList splits "a, b,,c" into sorted, unique, non-empty items.
func splitList(list string) []string {
	var out []string
	for _, item := range strings.Split(list, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
