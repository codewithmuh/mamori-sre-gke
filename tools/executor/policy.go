package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// faultKinds are the object kinds remove-fault may delete: exactly what the fault library
// (#23, chaos/faults/) creates. deploy/executor/rbac.yaml grants delete on the same list,
// and the admission policy there repeats the label check for every one of them.
var faultKinds = map[string]schema.GroupVersionResource{
	"virtualservice":      {Group: "networking.istio.io", Version: "v1", Resource: "virtualservices"},
	"sidecar":             {Group: "networking.istio.io", Version: "v1", Resource: "sidecars"},
	"authorizationpolicy": {Group: "security.istio.io", Version: "v1", Resource: "authorizationpolicies"},
	"deployment":          {Group: "apps", Version: "v1", Resource: "deployments"},
	"podchaos":            {Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "podchaos"},
	"networkchaos":        {Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "networkchaos"},
	"stresschaos":         {Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "stresschaos"},
}

func sortedFaultKinds() []string {
	kinds := make([]string, 0, len(faultKinds))
	for k := range faultKinds {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return kinds
}

// rollbackRefusal explains why rollback is never run here. Argo CD syncs deploy/ with
// automated sync and self-heal, so a `kubectl rollout undo` is reverted to Git's version on
// the next reconcile, and Argo CD's own rollback refuses to run while auto-sync is on.
const rollbackRefusal = "rollback is not run by the executor: Argo CD self-heal would undo a cluster-side rollback within seconds; " +
	"roll back by reverting the commit in Git (git revert, PR, merge) and let Argo CD sync it"

// Limits on the request, so a confused or injected agent cannot fill Slack or the state.
const (
	maxIDLen        = 128
	maxRationaleLen = 4000
	maxParams       = 4
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+-]*$`)

// proposalRequest is the POST /v1/proposals body (contract).
type proposalRequest struct {
	IncidentID      string `json:"incidentId"`
	InvestigationID string `json:"investigationId"`
	Action          Action `json:"action"`
	Rationale       string `json:"rationale"`
}

// validateRequest checks the shape of a request; it knows nothing about the configuration.
func validateRequest(r proposalRequest) *refusal {
	for _, f := range []struct{ name, value string }{
		{"incidentId", r.IncidentID},
		{"investigationId", r.InvestigationID},
	} {
		if f.value == "" || len(f.value) > maxIDLen || !idPattern.MatchString(f.value) {
			return refuse(reasonInvalid, "%s must be 1-%d characters of letters, digits and ._:@/+-", f.name, maxIDLen)
		}
	}
	if len(r.Rationale) > maxRationaleLen {
		return refuse(reasonInvalid, "rationale is longer than %d bytes", maxRationaleLen)
	}
	if strings.IndexFunc(r.Rationale, func(c rune) bool { return unicode.IsControl(c) && c != '\n' && c != '\t' }) >= 0 {
		return refuse(reasonInvalid, "rationale contains control characters")
	}
	a := r.Action
	if a.Type == "" || a.Namespace == "" || a.Target == "" {
		return refuse(reasonInvalid, "action needs type, namespace and target")
	}
	if len(a.Params) > maxParams {
		return refuse(reasonInvalid, "action.params has more than %d keys", maxParams)
	}
	return nil
}

// parseTarget splits "<kind>/<name>" (lowercase kind, as the agent and fault library write it).
func parseTarget(target string) (kind, name string, r *refusal) {
	kind, name, ok := strings.Cut(target, "/")
	if !ok || kind == "" || name == "" || strings.Contains(name, "/") {
		return "", "", refuse(reasonInvalid, "target %q must look like <kind>/<name>", target)
	}
	if !dns1123Subdomain.MatchString(name) {
		return "", "", refuse(reasonInvalid, "target name %q is not a valid Kubernetes name", name)
	}
	return kind, name, nil
}

// checkPolicy applies the allow-list and the bounds. It is pure, so it runs both when the
// proposal arrives and again after approval, against the configuration of that moment.
func checkPolicy(cfg *Config, a Action) *refusal {
	switch a.Type {
	case actionRollback:
		return refuse(reasonNotAllowed, rollbackRefusal)
	case actionRestart, actionScale, actionRemoveFault:
	default:
		return refuse(reasonNotAllowed, "action type %q is not known (allowed types: restart, scale, remove-fault)", a.Type)
	}
	if !cfg.allows(a.Type) {
		return refuse(reasonNotAllowed, "action type %q is not in the allow-list in %s", a.Type, configMapName)
	}
	if a.Namespace != targetNamespace {
		return refuse(reasonNotAllowed, "namespace %q is not allowed; the executor acts only in %q", a.Namespace, targetNamespace)
	}
	kind, name, r := parseTarget(a.Target)
	if r != nil {
		return r
	}

	switch a.Type {
	case actionRestart:
		if kind != "deployment" {
			return refuse(reasonNotAllowed, "restart works on deployments only, not %q", kind)
		}
		if !slices.Contains(cfg.Restart.Deployments, name) {
			return refuse(reasonNotAllowed, "deployment %q is not in restart.deployments", name)
		}
		if len(a.Params) > 0 {
			return refuse(reasonInvalid, "restart takes no params")
		}
	case actionScale:
		if kind != "deployment" {
			return refuse(reasonNotAllowed, "scale works on deployments only, not %q", kind)
		}
		bounds, ok := cfg.Scale[name]
		if !ok {
			return refuse(reasonNotAllowed, "deployment %q has no scale bounds, so it may not be scaled", name)
		}
		replicas, r := replicasParam(a.Params)
		if r != nil {
			return r
		}
		if replicas < bounds.Min || replicas > bounds.Max {
			return refuse(reasonOutOfBounds, "replicas %d is outside the bounds for %s (%d-%d)", replicas, name, bounds.Min, bounds.Max)
		}
	case actionRemoveFault:
		if !slices.Contains(cfg.RemoveFault.Kinds, kind) {
			return refuse(reasonNotAllowed, "kind %q is not in removeFault.kinds", kind)
		}
		if len(a.Params) > 0 {
			return refuse(reasonInvalid, "remove-fault takes no params")
		}
	}
	return nil
}

// replicasParam reads params.replicas, which must be a whole number. Anything else in
// params is refused, so the agent cannot smuggle in options the code would ignore.
func replicasParam(params map[string]any) (int32, *refusal) {
	for k := range params {
		if k != "replicas" {
			return 0, refuse(reasonInvalid, "scale takes only params.replicas, not %q", k)
		}
	}
	v, ok := params["replicas"]
	if !ok {
		return 0, refuse(reasonInvalid, "scale needs params.replicas")
	}
	var f float64
	switch n := v.(type) {
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return 0, refuse(reasonInvalid, "params.replicas %q is not a number", n)
		}
		f = parsed
	case float64:
		f = n
	case int:
		f = float64(n)
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return 0, refuse(reasonInvalid, "params.replicas must be a number, not %T", v)
	}
	if f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, refuse(reasonInvalid, "params.replicas %v is not a whole number", f)
	}
	if f < 0 || f > math.MaxInt32 {
		return 0, refuse(reasonOutOfBounds, "params.replicas %v is out of range", f)
	}
	return int32(f), nil
}

// describe is a short human line for Slack and logs.
func describe(a Action) string {
	switch a.Type {
	case actionScale:
		if r, err := replicasParam(a.Params); err == nil {
			return fmt.Sprintf("scale %s in %s to %d replicas", a.Target, a.Namespace, r)
		}
	case actionRestart:
		return fmt.Sprintf("restart %s in %s (rolling)", a.Target, a.Namespace)
	case actionRemoveFault:
		return fmt.Sprintf("delete fault object %s in %s", a.Target, a.Namespace)
	}
	return fmt.Sprintf("%s %s in %s", a.Type, a.Target, a.Namespace)
}
