package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Regenerate the golden files after an intended change:
//
//	go test ./... -run TestGolden -update
var update = flag.Bool("update", false, "rewrite testdata/golden from the current output")

const (
	fixtureRoot = "testdata/repo"
	goldenRoot  = "testdata/golden"
)

var fixtureInputs = Inputs{SLOs: "slo/slos.yaml", CUJMap: "slo/cuj-map.yaml"}

func quietLogger() *slog.Logger { return newLogger(io.Discard) }

func fixtureFiles(t *testing.T) []File {
	t.Helper()
	files, err := generate(fixtureRoot, fixtureInputs, quietLogger())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return files
}

func fixtureModel(t *testing.T) *Model {
	t.Helper()
	spec, err := LoadSpec(filepath.Join(fixtureRoot, "slo/slos.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadCUJMap(filepath.Join(fixtureRoot, "slo/cuj-map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	model, _, err := BuildModel(spec, m, fixtureInputs)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestGolden(t *testing.T) {
	files := fixtureFiles(t)
	if *update {
		if err := os.RemoveAll(goldenRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := Sync(goldenRoot, files, false); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]bool{}
	for _, f := range files {
		want[f.Path] = true
		golden, err := os.ReadFile(filepath.Join(goldenRoot, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("%s: %v (run: go test ./... -run TestGolden -update)", f.Path, err)
		}
		if !bytes.Equal(golden, f.Content) {
			t.Errorf("%s differs from the golden file; run: go test ./... -run TestGolden -update, then review the diff", f.Path)
		}
	}
	err := filepath.WalkDir(goldenRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(goldenRoot, p)
		if !want[filepath.ToSlash(rel)] {
			t.Errorf("golden file %s is no longer generated", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The output must not depend on map iteration order or anything else that varies between runs.
func TestDeterministic(t *testing.T) {
	a := fixtureFiles(t)
	for i := 0; i < 5; i++ {
		b := fixtureFiles(t)
		if len(a) != len(b) {
			t.Fatalf("run %d: %d files, want %d", i, len(b), len(a))
		}
		for k := range a {
			if a[k].Path != b[k].Path || !bytes.Equal(a[k].Content, b[k].Content) {
				t.Fatalf("run %d: %s differs between runs", i, a[k].Path)
			}
		}
	}
}

func fileByPath(t *testing.T, files []File, path string) []byte {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f.Content
		}
	}
	t.Fatalf("%s not generated", path)
	return nil
}

func TestRulesManifests(t *testing.T) {
	files := fixtureFiles(t)
	type rulesDoc struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			Groups []struct {
				Name     string `yaml:"name"`
				Interval string `yaml:"interval"`
				Rules    []struct {
					Record string            `yaml:"record"`
					Expr   string            `yaml:"expr"`
					Labels map[string]string `yaml:"labels"`
				} `yaml:"rules"`
			} `yaml:"groups"`
		} `yaml:"spec"`
	}
	cases := map[string]struct {
		kind, namespace string
		rules           int
	}{
		// 4 journeys x (availability + latency) x 2 windows.
		pathProberRules: {"Rules", "prober", 16},
		// 3 windows x (1 availability + 2 thresholds) per service, plus 4 journeys x 2 SLIs x 3 windows.
		pathMeshRules: {"ClusterRules", "", 9 + 24},
	}
	for path, want := range cases {
		var doc rulesDoc
		if err := yaml.Unmarshal(fileByPath(t, files, path), &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if doc.APIVersion != "monitoring.googleapis.com/v1" || doc.Kind != want.kind || doc.Metadata.Namespace != want.namespace {
			t.Errorf("%s: got %s %s in %q", path, doc.APIVersion, doc.Kind, doc.Metadata.Namespace)
		}
		if len(doc.Spec.Groups) != 1 || doc.Spec.Groups[0].Interval != "1m" {
			t.Fatalf("%s: want one group evaluated every 1m, got %+v", path, doc.Spec.Groups)
		}
		rules := doc.Spec.Groups[0].Rules
		if len(rules) != want.rules {
			t.Errorf("%s: %d rules, want %d", path, len(rules), want.rules)
		}
		for _, r := range rules {
			if !strings.HasPrefix(r.Record, "mamori:") || r.Expr == "" {
				t.Errorf("%s: bad rule %+v", path, r)
			}
			if strings.Count(r.Expr, "(") != strings.Count(r.Expr, ")") || strings.Count(r.Expr, "{") != strings.Count(r.Expr, "}") {
				t.Errorf("%s: unbalanced brackets in %s", path, r.Expr)
			}
			// Managed Prometheus adds these itself and rejects rules that set them.
			for _, l := range []string{"project_id", "location", "cluster", "namespace"} {
				if _, ok := r.Labels[l]; ok {
					t.Errorf("%s: rule %s sets reserved label %s", path, r.Record, l)
				}
			}
		}
	}
	kust := string(fileByPath(t, files, pathKustomization))
	if !strings.Contains(kust, "- mesh-rules.yaml") || !strings.Contains(kust, "- prober-rules.yaml") {
		t.Errorf("kustomization does not list both rule files:\n%s", kust)
	}
}

func TestMeshComposite(t *testing.T) {
	m := fixtureModel(t)
	browse := m.Journeys[0]
	if got := strings.Join(browse.MeshServices, ","); got != "currencyservice,frontend,productcatalogservice" {
		t.Errorf("browse mesh services = %s", got)
	}
	addToCart := m.Journeys[1]
	if got := strings.Join(addToCart.TCPSkipped, ","); got != "redis-cart" {
		t.Errorf("add-to-cart TCP skipped = %s", got)
	}
	expr := cujMeshAvailabilityExpr(browse, m.Spec.Mesh.SLORateWindow)
	for _, want := range []string{"exp(sum(ln(", `mamori:service_mesh_availability:ratio_rate2m{destination_service_name=~"currencyservice|frontend|productcatalogservice"}`} {
		if !strings.Contains(expr, want) {
			t.Errorf("composite availability %q lacks %q", expr, want)
		}
	}
	lat := cujMeshLatencyExpr(m.Journeys[3], m.Spec.Mesh.SLORateWindow)
	if !strings.Contains(lat, `min(`) || !strings.Contains(lat, `threshold_ms="1000"`) {
		t.Errorf("checkout latency composite = %q", lat)
	}
	if got := m.ThresholdServices[1000]; strings.Join(got, ",") != "cartservice,checkoutservice,currencyservice,frontend,paymentservice,productcatalogservice,shippingservice" {
		t.Errorf("services at 1000 ms = %v", got)
	}
}

func TestLabTerraform(t *testing.T) {
	var root struct {
		Comment  string                               `json:"//"`
		Resource map[string]map[string]map[string]any `json:"resource"`
	}
	if err := json.Unmarshal(fileByPath(t, fixtureFiles(t), pathLabTF), &root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(root.Comment, "DO NOT EDIT") {
		t.Errorf("missing generated notice: %q", root.Comment)
	}
	r := root.Resource
	counts := map[string]int{
		"google_monitoring_custom_service": 4,
		"google_monitoring_slo":            16,
		// 16 SLOs x 3 tiers + 4 no-data policies.
		"google_monitoring_alert_policy": 52,
		"google_monitoring_dashboard":    1,
	}
	for typ, n := range counts {
		if len(r[typ]) != n {
			t.Errorf("%d %s, want %d", len(r[typ]), typ, n)
		}
	}
	for name, p := range r["google_monitoring_alert_policy"] {
		if !strings.HasPrefix(name, "slo_") {
			t.Errorf("%s: name lacks the slo_ prefix", name)
		}
		doc := p["documentation"].([]any)[0].(map[string]any)
		if !strings.Contains(doc["content"].(string), "/docs/runbooks/cuj-") {
			t.Errorf("%s: no runbook link", name)
		}
		labels := p["user_labels"].(map[string]any)
		for _, k := range []string{"cuj", "signal", "sli", "alert", "burn", "severity", "generator"} {
			if labels[k] == nil {
				t.Errorf("%s: missing user label %s", name, k)
			}
		}
		page := labels["severity"] == severityPage
		if page != (p["severity"] == "CRITICAL") || page != (p["notification_channels"] == "${local.incident_notification_channels}") {
			t.Errorf("%s: severity %v, channels %v do not match label %v", name, p["severity"], p["notification_channels"], labels["severity"])
		}
	}
	fast := r["google_monitoring_alert_policy"]["slo_checkout_prober_availability_fast"]
	conds := fast["conditions"].([]any)
	if len(conds) != 2 || fast["combiner"] != "AND" {
		t.Fatalf("fast policy: want 2 conditions joined by AND, got %v", fast)
	}
	ct := conds[1].(map[string]any)["condition_threshold"].([]any)[0].(map[string]any)
	if ct["filter"] != `select_slo_burn_rate("${google_monitoring_slo.slo_checkout_prober_availability.name}", "300s")` || ct["threshold_value"] != 13.44 {
		t.Errorf("short-window condition = %v", ct)
	}
	dash := r["google_monitoring_dashboard"]["slo_overview"]["dashboard_json"].(string)
	var d map[string]any
	if err := json.Unmarshal([]byte(dash), &d); err != nil {
		t.Errorf("dashboard_json is not JSON: %v", err)
	}
}

func TestDatadogTerraform(t *testing.T) {
	var root struct {
		Resource map[string]map[string]map[string]any `json:"resource"`
	}
	if err := json.Unmarshal(fileByPath(t, fixtureFiles(t), pathDatadogTF), &root); err != nil {
		t.Fatal(err)
	}
	slos := root.Resource["datadog_service_level_objective"]
	monitors := root.Resource["datadog_monitor"]
	if len(slos) != 8 || len(monitors) != 12 {
		t.Fatalf("got %d SLOs and %d monitors, want 8 and 12", len(slos), len(monitors))
	}
	mon := slos["slo_add_to_cart_synthetics_availability"]
	if ids := mon["monitor_ids"].([]any); len(ids) != 1 || ids[0] != "${datadog_synthetics_test.cuj_add_to_cart.monitor_id}" {
		t.Errorf("monitor ids = %v", mon["monitor_ids"])
	}
	burn := monitors["slo_checkout_synthetics_slow"]
	want := `burn_rate("${datadog_service_level_objective.slo_checkout_synthetics_runs.id}").over("7d").long_window("24h").short_window("2h") > 2.8`
	if burn["query"] != want || burn["priority"] != "3" {
		t.Errorf("slow burn monitor = %v", burn)
	}
}

func TestDatadogBurnRateDisabled(t *testing.T) {
	m := fixtureModel(t)
	m.Spec.Datadog.BurnRate.Enabled = false
	f, err := m.renderDatadog()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.Content, []byte("datadog_monitor")) || bytes.Contains(f.Content, []byte("_runs")) {
		t.Errorf("burn-rate resources generated although disabled:\n%s", f.Content)
	}
}

func TestTiersNarrowedToOneSignal(t *testing.T) {
	m := fixtureModel(t)
	m.Spec.Alerting.Tiers[0].Signals = []string{signalMesh}
	if got := len(m.TiersFor(signalProber)); got != 2 {
		t.Errorf("prober tiers = %d, want 2", got)
	}
	if got := len(m.TiersFor(signalMesh)); got != 3 {
		t.Errorf("mesh tiers = %d, want 3", got)
	}
}

func TestCatalog(t *testing.T) {
	c := string(fileByPath(t, fixtureFiles(t), pathCatalog))
	for _, want := range []string{
		"| fast | page | 13.44 | 1h | 5m | 2 % |",
		"| slow | ticket | 2.8 | 24h | 2h | 10 % |",
		"201 failed runs (at one run a minute)",
		"403 bad 1m windows",
		"(left out, plain TCP: redis-cart)",
		"`CUJ checkout: mesh latency medium burn` (page, burn rate > 5.6)",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("catalog lacks %q", want)
		}
	}
}

// The committed spec must always be valid.
func TestRepositorySpec(t *testing.T) {
	path := filepath.Join("..", "..", "slo", "slos.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("not inside the repository")
	}
	if _, err := LoadSpec(path); err != nil {
		t.Fatal(err)
	}
}
