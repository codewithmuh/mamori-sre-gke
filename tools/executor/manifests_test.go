package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The same lists live in three places: the executor's code, executor-config, and the RBAC and
// admission policies in deploy/executor. These tests fail when they drift apart, so a change
// to one cannot silently widen (or break) another.

const deployDir = "../../deploy/executor"

func readDocs(t *testing.T, file string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(deployDir, file))
	if os.IsNotExist(err) {
		t.Skipf("%s not found; run the tests from a checkout of the whole repository", file)
	}
	if err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	for _, d := range strings.Split(string(raw), "\n---") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if m != nil {
			docs = append(docs, m)
		}
	}
	return docs
}

// field walks nested maps: field(doc, "spec", "validations").
func field(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func items(v any) []any {
	l, _ := v.([]any)
	return l
}

func strs(v any) []string {
	var out []string
	for _, x := range items(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func find(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == kind && field(d, "metadata", "name") == name {
			return d
		}
	}
	t.Fatalf("no %s %s", kind, name)
	return nil
}

var quoted = regexp.MustCompile(`'([^']*)'`)

// namesIn returns the list in a policy's "request.name in [...]" validation.
func namesIn(t *testing.T, policy map[string]any) []string {
	t.Helper()
	for _, v := range items(field(policy, "spec", "validations")) {
		expr, _ := field(v, "expression").(string)
		if _, list, ok := strings.Cut(expr, "request.name in ["); ok {
			list, _, _ = strings.Cut(list, "]")
			var names []string
			for _, m := range quoted.FindAllStringSubmatch(list, -1) {
				names = append(names, m[1])
			}
			slices.Sort(names)
			return names
		}
	}
	t.Fatalf("policy %v has no request.name list", field(policy, "metadata", "name"))
	return nil
}

func shippedConfig(t *testing.T) *Config {
	t.Helper()
	cm := find(t, readDocs(t, "configmap-config.yaml"), "ConfigMap", configMapName)
	data := map[string]string{}
	for k, v := range field(cm, "data").(map[string]any) {
		data[k], _ = v.(string)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		t.Fatalf("shipped executor-config is invalid: %v", err)
	}
	if cfg.Enabled {
		t.Error("executor-config must ship with the kill switch on (enabled: \"false\")")
	}
	return cfg
}

func TestAdmissionNamesMatchConfig(t *testing.T) {
	cfg := shippedConfig(t)
	policies := readDocs(t, "admission-policy.yaml")

	restart := slices.Clone(cfg.Restart.Deployments)
	slices.Sort(restart)
	if got := namesIn(t, find(t, policies, "ValidatingAdmissionPolicy", "mamori-executor-restart-only")); !slices.Equal(got, restart) {
		t.Errorf("restart-only policy names %v, executor-config restart.deployments %v", got, restart)
	}

	var scale []string
	for name := range cfg.Scale {
		scale = append(scale, name)
	}
	slices.Sort(scale)
	if got := namesIn(t, find(t, policies, "ValidatingAdmissionPolicy", "mamori-executor-scale-bounds")); !slices.Equal(got, scale) {
		t.Errorf("scale-bounds policy names %v, executor-config scale %v", got, scale)
	}
	for name, b := range cfg.Scale {
		if b.Max > hardMaxReplicas {
			t.Errorf("scale.%s max %d is above the policy's ceiling %d", name, b.Max, hardMaxReplicas)
		}
	}
}

func TestFaultKindsMatchRBACAndPolicy(t *testing.T) {
	var code []string
	for _, gvr := range faultKinds {
		code = append(code, gvr.Group+"/"+gvr.Resource)
	}
	slices.Sort(code)

	// RBAC: every fault kind needs get and delete in boutique, and nothing else may be deletable.
	var role map[string]any
	for _, d := range readDocs(t, "rbac.yaml") {
		if d["kind"] == "Role" && field(d, "metadata", "namespace") == targetNamespace {
			role = d
		}
	}
	if role == nil {
		t.Fatal("no Role in boutique in rbac.yaml")
	}
	verbs := map[string][]string{}
	for _, r := range items(field(role, "rules")) {
		for _, g := range strs(field(r, "apiGroups")) {
			for _, res := range strs(field(r, "resources")) {
				verbs[g+"/"+res] = append(verbs[g+"/"+res], strs(field(r, "verbs"))...)
			}
		}
	}
	var deletable []string
	for gr, vs := range verbs {
		if slices.Contains(vs, "delete") {
			deletable = append(deletable, gr)
			if !slices.Contains(vs, "get") {
				t.Errorf("RBAC lets the executor delete %s without get, so it could not check the label", gr)
			}
		}
	}
	slices.Sort(deletable)
	if !slices.Equal(deletable, code) {
		t.Errorf("RBAC delete rights %v, faultKinds %v", deletable, code)
	}

	// The fault-delete policy must cover every kind the executor may delete.
	var guarded []string
	policy := find(t, readDocs(t, "admission-policy.yaml"), "ValidatingAdmissionPolicy", "mamori-executor-fault-deletes")
	for _, rule := range items(field(policy, "spec", "matchConstraints", "resourceRules")) {
		for _, g := range strs(field(rule, "apiGroups")) {
			for _, res := range strs(field(rule, "resources")) {
				guarded = append(guarded, g+"/"+res)
			}
		}
	}
	slices.Sort(guarded)
	if !slices.Equal(guarded, code) {
		t.Errorf("fault-delete policy covers %v, faultKinds %v", guarded, code)
	}

	// The shipped config allows exactly the kinds the code knows.
	kinds := slices.Clone(shippedConfig(t).RemoveFault.Kinds)
	slices.Sort(kinds)
	if !slices.Equal(kinds, sortedFaultKinds()) {
		t.Errorf("removeFault.kinds %v, faultKinds %v", kinds, sortedFaultKinds())
	}
}
