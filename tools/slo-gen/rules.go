package main

import (
	"bytes"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Kubernetes objects of Managed Service for Prometheus (monitoring.googleapis.com/v1).
type k8sObject struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   objectMeta `yaml:"metadata"`
	Spec       rulesSpec  `yaml:"spec"`
}

type objectMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

type rulesSpec struct {
	Groups []ruleGroup `yaml:"groups"`
}

type ruleGroup struct {
	Name     string `yaml:"name"`
	Interval string `yaml:"interval"`
	Rules    []rule `yaml:"rules"`
}

// rule is a recording rule with a comment above it.
type rule struct {
	Comment string
	Record  string
	Expr    string
	Labels  map[string]string
}

// MarshalYAML writes the expression as a literal block, so multi-line PromQL
// stays readable, and the comment above the rule.
func (r rule) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode, HeadComment: r.Comment}
	add := func(k string, v *yaml.Node) {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, v)
	}
	add("record", &yaml.Node{Kind: yaml.ScalarNode, Value: r.Record})
	add("expr", &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.LiteralStyle, Value: r.Expr})
	if len(r.Labels) > 0 {
		var labels yaml.Node
		if err := labels.Encode(r.Labels); err != nil {
			return nil, err
		}
		add("labels", &labels)
	}
	return n, nil
}

var commonLabels = map[string]string{
	"app.kubernetes.io/name":    "slo",
	"app.kubernetes.io/part-of": "mamori",
}

// proberRules are namespaced Rules in the prober's namespace. Each rule
// carries its journey as a static label too, so rules that share a record
// name differ in their labels (promtool flags them as duplicates otherwise).
// Managed Prometheus evaluates them over that namespace's data only, so
// another workload exporting the same metric names cannot leak in.
func (m *Model) proberRules() k8sObject {
	g := ruleGroup{Name: "slo-prober", Interval: m.Spec.GCP.RuleInterval.String()}
	for _, j := range m.Journeys {
		for _, w := range m.Spec.Prober.RecordWindows {
			g.Rules = append(g.Rules, rule{
				Comment: fmt.Sprintf("%s: share of scheduled prober runs that succeeded, over %s.", j.ID, w),
				Record:  recCUJ(signalProber, sliAvailability, w),
				Expr:    proberAvailabilityExpr(j, w),
				Labels:  map[string]string{"cuj": j.ID},
			})
		}
		for _, w := range m.Spec.Prober.RecordWindows {
			g.Rules = append(g.Rules, rule{
				Comment: fmt.Sprintf("%s: share of successful %s steps under %d ms, over %s.",
					j.ID, strings.Join(j.ProberSteps, "/"), j.Latency.ThresholdMs, w),
				Record: recCUJ(signalProber, sliLatency, w),
				Expr:   proberLatencyExpr(j, w),
				Labels: map[string]string{"cuj": j.ID},
			})
		}
	}
	return k8sObject{
		APIVersion: "monitoring.googleapis.com/v1",
		Kind:       "Rules",
		Metadata:   objectMeta{Name: "slo-prober", Namespace: m.Spec.Prober.Namespace, Labels: commonLabels},
		Spec:       rulesSpec{Groups: []ruleGroup{g}},
	}
}

// meshRules are ClusterRules: the Istio metrics come from sidecars in the
// shop's namespace and from the ingress gateway's namespace. One group, so
// the per-service series are fresh when the journey composites read them
// (rules in a group run in order; groups run concurrently).
func (m *Model) meshRules() k8sObject {
	g := ruleGroup{Name: "slo-mesh", Interval: m.Spec.GCP.RuleInterval.String()}
	ws := m.meshWindows()
	for _, w := range ws {
		g.Rules = append(g.Rules, rule{
			Comment: fmt.Sprintf("Per service: share of requests answered below 500 with gRPC status OK, over %s (client-side view).", w),
			Record:  recService(sliAvailability, w),
			Expr:    m.serviceAvailabilityExpr(w),
		})
		for _, t := range m.Thresholds {
			g.Rules = append(g.Rules, rule{
				Comment: fmt.Sprintf("Per service (journeys with a %d ms threshold): share of requests faster than %d ms, over %s.", t, t, w),
				Record:  recService(sliLatency, w),
				Expr:    m.serviceLatencyExpr(t, m.ThresholdServices[t], w),
				Labels:  map[string]string{"threshold_ms": fmt.Sprint(t)},
			})
		}
	}
	for _, j := range m.Journeys {
		for _, w := range ws {
			g.Rules = append(g.Rules, rule{
				Comment: fmt.Sprintf("%s: availability = product of the success ratios of %s, over %s.", j.ID, strings.Join(j.MeshServices, ", "), w),
				Record:  recCUJ(signalMesh, sliAvailability, w),
				Expr:    cujMeshAvailabilityExpr(j, w),
				Labels:  map[string]string{"cuj": j.ID},
			})
			g.Rules = append(g.Rules, rule{
				Comment: fmt.Sprintf("%s: latency = lowest share under %d ms among %s, over %s.", j.ID, j.Latency.ThresholdMs, strings.Join(j.MeshServices, ", "), w),
				Record:  recCUJ(signalMesh, sliLatency, w),
				Expr:    cujMeshLatencyExpr(j, w),
				Labels:  map[string]string{"cuj": j.ID},
			})
		}
	}
	return k8sObject{
		APIVersion: "monitoring.googleapis.com/v1",
		Kind:       "ClusterRules",
		Metadata:   objectMeta{Name: "slo-mesh", Labels: commonLabels},
		Spec:       rulesSpec{Groups: []ruleGroup{g}},
	}
}

func (m *Model) yamlHeader(extra ...string) []byte {
	var b bytes.Buffer
	for _, l := range append(m.HeaderLines(), extra...) {
		b.WriteString("# " + l + "\n")
	}
	return b.Bytes()
}

func encodeYAML(header []byte, v any) ([]byte, error) {
	var b bytes.Buffer
	b.Write(header)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

const (
	pathKustomization = "deploy/slo/kustomization.yaml"
	pathMeshRules     = "deploy/slo/mesh-rules.yaml"
	pathProberRules   = "deploy/slo/prober-rules.yaml"
)

func (m *Model) renderRules() ([]File, error) {
	prober, err := encodeYAML(m.yamlHeader(
		"Prober SLIs per journey (source=\"scheduled\"). Managed Prometheus scopes these rules to this namespace."),
		m.proberRules())
	if err != nil {
		return nil, err
	}
	mesh, err := encodeYAML(m.yamlHeader(
		"Mesh SLIs: per-service success and latency from Istio's client-side metrics, and per journey the",
		"composite of its critical services (docs/slo/slo-design.md)."),
		m.meshRules())
	if err != nil {
		return nil, err
	}
	kust, err := encodeYAML(m.yamlHeader("Synced by the Argo CD Application deploy/apps/slo.yaml."), map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{"mesh-rules.yaml", "prober-rules.yaml"},
	})
	if err != nil {
		return nil, err
	}
	return []File{
		{Path: pathKustomization, Content: kust},
		{Path: pathMeshRules, Content: mesh},
		{Path: pathProberRules, Content: prober},
	}, nil
}
