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
	"time"

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
		// group name -> interval and number of rules
		groups map[string]string
		rules  map[string]int
	}{
		// Per window: 4 journeys x (availability + latency).
		pathProberRules: {"Rules", "prober",
			map[string]string{"slo-prober-5m": "1m", "slo-prober-1h": "5m"},
			map[string]int{"slo-prober-5m": 8, "slo-prober-1h": 8}},
		// Per window: every service (1 availability + 2 thresholds), plus 4 journeys x 2 SLIs.
		pathMeshRules: {"ClusterRules", "",
			map[string]string{"slo-mesh-2m": "1m", "slo-mesh-5m": "1m", "slo-mesh-1h": "5m"},
			map[string]int{"slo-mesh-2m": 11, "slo-mesh-5m": 11, "slo-mesh-1h": 11}},
	}
	for path, want := range cases {
		var doc rulesDoc
		if err := yaml.Unmarshal(fileByPath(t, files, path), &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if doc.APIVersion != "monitoring.googleapis.com/v1" || doc.Kind != want.kind || doc.Metadata.Namespace != want.namespace {
			t.Errorf("%s: got %s %s in %q", path, doc.APIVersion, doc.Kind, doc.Metadata.Namespace)
		}
		if len(doc.Spec.Groups) != len(want.groups) {
			t.Fatalf("%s: %d groups, want %d", path, len(doc.Spec.Groups), len(want.groups))
		}
		for _, g := range doc.Spec.Groups {
			if g.Interval != want.groups[g.Name] || len(g.Rules) != want.rules[g.Name] {
				t.Errorf("%s: group %s every %s with %d rules, want every %s with %d", path, g.Name, g.Interval, len(g.Rules), want.groups[g.Name], want.rules[g.Name])
			}
			for _, r := range g.Rules {
				checkRule(t, path, r.Record, r.Expr, r.Labels)
			}
		}
	}
	kust := string(fileByPath(t, files, pathKustomization))
	if !strings.Contains(kust, "- mesh-rules.yaml") || !strings.Contains(kust, "- prober-rules.yaml") {
		t.Errorf("kustomization does not list both rule files:\n%s", kust)
	}
}

func checkRule(t *testing.T, path, record, expr string, labels map[string]string) {
	t.Helper()
	if !strings.HasPrefix(record, "mamori:") || expr == "" {
		t.Errorf("%s: bad rule %s: %q", path, record, expr)
	}
	if strings.Count(expr, "(") != strings.Count(expr, ")") || strings.Count(expr, "{") != strings.Count(expr, "}") {
		t.Errorf("%s: unbalanced brackets in %s", path, expr)
	}
	// Managed Prometheus adds these itself and rejects rules that set them.
	for _, l := range []string{"project_id", "location", "cluster", "namespace"} {
		if _, ok := labels[l]; ok {
			t.Errorf("%s: rule %s sets reserved label %s", path, record, l)
		}
	}
}

func TestMeshComposite(t *testing.T) {
	m := fixtureModel(t)
	browse := m.Journeys[0]
	// The fixture map lists frontend as critical for browse; the spec excludes it.
	if got := strings.Join(browse.MeshServices, ","); got != "currencyservice,productcatalogservice" || strings.Join(browse.Excluded, ",") != "frontend" {
		t.Errorf("browse mesh services = %s, excluded = %v", got, browse.Excluded)
	}
	addToCart := m.Journeys[1]
	if got := strings.Join(addToCart.TCPSkipped, ","); got != "redis-cart" {
		t.Errorf("add-to-cart TCP skipped = %s", got)
	}
	expr := cujMeshAvailabilityExpr(browse, m.Spec.Mesh.SLORateWindow)
	for _, want := range []string{"exp(sum(ln(", `mamori:service_mesh_availability:ratio_rate2m{destination_service_name=~"currencyservice|productcatalogservice"}`} {
		if !strings.Contains(expr, want) {
			t.Errorf("composite availability %q lacks %q", expr, want)
		}
	}
	lat := cujMeshLatencyExpr(m.Journeys[3], m.Spec.Mesh.SLORateWindow)
	if !strings.Contains(lat, `min(`) || !strings.Contains(lat, `threshold_ms="1000"`) {
		t.Errorf("checkout latency composite = %q", lat)
	}
	// Every service of the namespace is recorded, with the wider set of good gRPC statuses.
	svc := m.serviceAvailabilityExpr(m.Spec.Mesh.SLORateWindow)
	if strings.Contains(svc, "destination_service_name=~") || !strings.Contains(svc, `grpc_response_status=~"0|3|5|"`) {
		t.Errorf("per-service availability = %q", svc)
	}
	if strings.Join(intsToStrings(m.Thresholds), ",") != "500,1000" {
		t.Errorf("thresholds = %v", m.Thresholds)
	}
}

func TestLabTerraform(t *testing.T) {
	var root struct {
		Comment  string                               `json:"//"`
		Variable map[string]map[string]any            `json:"variable"`
		Resource map[string]map[string]map[string]any `json:"resource"`
	}
	if err := json.Unmarshal(fileByPath(t, fixtureFiles(t), pathLabTF), &root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(root.Comment, "DO NOT EDIT") {
		t.Errorf("missing generated notice: %q", root.Comment)
	}
	if v := root.Variable[varSLOEnabled]; v["type"] != "bool" || v["default"] != false || v["description"] == "" {
		t.Errorf("variable slo_enabled = %v", v)
	}
	r := root.Resource
	counts := map[string]int{
		"google_monitoring_custom_service": 4,
		"google_monitoring_slo":            16,
		// 16 SLOs x 3 tiers + 2 no-data policies.
		"google_monitoring_alert_policy": 50,
		"google_monitoring_dashboard":    1,
	}
	for typ, n := range counts {
		if len(r[typ]) != n {
			t.Errorf("%d %s, want %d", len(r[typ]), typ, n)
		}
		// Nothing may exist before the metrics do.
		for name, res := range r[typ] {
			if res["count"] != countGate {
				t.Errorf("%s.%s is not gated on var.slo_enabled", typ, name)
			}
		}
	}
	pages := 0
	for name, p := range r["google_monitoring_alert_policy"] {
		if !strings.HasPrefix(name, "slo_") {
			t.Errorf("%s: name lacks the slo_ prefix", name)
		}
		doc := p["documentation"].([]any)[0].(map[string]any)
		if !strings.Contains(doc["content"].(string), "/docs/runbooks/") {
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
		if page {
			pages++
			if labels["signal"] != signalProber || labels["burn"] == "slow" {
				t.Errorf("%s pages, but only the prober's fast and medium tiers (and prober no data) should", name)
			}
		}
	}
	// 4 journeys x 2 SLIs x (fast + medium), plus prober no data.
	if pages != 17 {
		t.Errorf("%d page policies, want 17", pages)
	}
	fast := r["google_monitoring_alert_policy"]["slo_checkout_prober_availability_fast"]
	conds := fast["conditions"].([]any)
	if len(conds) != 2 || fast["combiner"] != "AND" {
		t.Fatalf("fast policy: want 2 conditions joined by AND, got %v", fast)
	}
	ct := conds[1].(map[string]any)["condition_threshold"].([]any)[0].(map[string]any)
	if ct["filter"] != `select_slo_burn_rate("${google_monitoring_slo.slo_checkout_prober_availability[0].name}", "300s")` || ct["threshold_value"] != 13.44 {
		t.Errorf("short-window condition = %v", ct)
	}
	latFast := r["google_monitoring_alert_policy"]["slo_browse_prober_latency_fast"]["conditions"].([]any)[0].(map[string]any)
	if v := latFast["condition_threshold"].([]any)[0].(map[string]any)["threshold_value"]; v != 6.72 {
		t.Errorf("latency fast burn = %v, want 6.72", v)
	}
	for name, want := range map[string]string{
		"slo_prober_no_data": `absent_over_time(mamori_cuj_probe_total{cluster="mamori",namespace="prober",source="scheduled"}[10m])`,
		"slo_mesh_no_data":   `absent_over_time(mamori:cuj_mesh_availability:ratio_rate2m{cluster="mamori"}[10m])`,
	} {
		c := r["google_monitoring_alert_policy"][name]["conditions"].([]any)[0].(map[string]any)
		q := c["condition_prometheus_query_language"].([]any)[0].(map[string]any)
		if q["query"] != want || q["disable_metric_validation"] != true {
			t.Errorf("%s condition = %v", name, q)
		}
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
	// 4 journeys x (monitor SLO + runs SLO); 4 journeys x (medium + slow).
	if len(slos) != 8 || len(monitors) != 8 {
		t.Fatalf("got %d SLOs and %d monitors, want 8 and 8", len(slos), len(monitors))
	}
	mon := slos["slo_add_to_cart_synthetics_availability"]
	if ids := mon["monitor_ids"].([]any); len(ids) != 1 || ids[0] != "${datadog_synthetics_test.cuj_add_to_cart.monitor_id}" {
		t.Errorf("monitor ids = %v", mon["monitor_ids"])
	}
	want := map[string]string{
		"slo_checkout_synthetics_medium": `burn_rate("${datadog_service_level_objective.slo_checkout_synthetics_runs.id}").over("7d").long_window("6h").short_window("30m") > 5.6`,
		"slo_checkout_synthetics_slow":   `burn_rate("${datadog_service_level_objective.slo_checkout_synthetics_runs.id}").over("7d").long_window("24h").short_window("2h") > 2.8`,
	}
	for name, q := range want {
		// Synthetics are not a paging signal: every monitor is a ticket (priority 3).
		if monitors[name]["query"] != q || monitors[name]["priority"] != "3" {
			t.Errorf("%s = %v", name, monitors[name])
		}
	}
}

func TestDatadogMinShortWindow(t *testing.T) {
	m := fixtureModel(t)
	m.Spec.Datadog.MinShortWindow = Duration(45 * time.Minute)
	tiers := m.datadogTiers()
	if len(tiers) != 2 || tiers[0].ShortWindow.String() != "45m" || tiers[1].ShortWindow.String() != "2h" {
		t.Errorf("datadog tiers = %+v", tiers)
	}
	// The Cloud Monitoring tiers are untouched.
	if m.Spec.Alerting.AvailabilityTiers[1].ShortWindow.String() != "30m" {
		t.Error("datadogTiers changed the spec")
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

func TestSeverity(t *testing.T) {
	m := fixtureModel(t)
	fast, slow := m.Spec.Alerting.AvailabilityTiers[0], m.Spec.Alerting.AvailabilityTiers[2]
	cases := []struct {
		signal string
		tier   TierSpec
		want   string
	}{
		{signalProber, fast, severityPage},
		{signalProber, slow, severityTicket},
		{signalMesh, fast, severityTicket},
		{signalSynthetics, fast, severityTicket},
	}
	for _, c := range cases {
		if got := m.Severity(c.signal, c.tier); got != c.want {
			t.Errorf("Severity(%s, %s) = %s, want %s", c.signal, c.tier.Name, got, c.want)
		}
	}
	m.Spec.Alerting.PageSignals = []string{signalProber, signalMesh}
	if m.Severity(signalMesh, fast) != severityPage {
		t.Error("mesh in pageSignals should page")
	}
	if got := len(m.TiersFor(sliLatency)); got != 3 || m.TiersFor(sliLatency)[0].BudgetConsumed != 0.01 {
		t.Errorf("latency tiers = %v", m.TiersFor(sliLatency))
	}
}

func TestJourneyWithoutMeshServices(t *testing.T) {
	spec := loadFixtureSpec(t)
	m, err := LoadCUJMap(filepath.Join(fixtureRoot, "slo/cuj-map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m.Journeys[0].Critical = []string{"redis-cart"} // browse: TCP only
	model, warnings, err := BuildModel(spec, m, fixtureInputs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), `"browse" has no critical service with request metrics`) {
		t.Errorf("warnings = %v", warnings)
	}
	if b := model.Journeys[0]; b.HasMesh() || len(b.SLOs) != 2 {
		t.Errorf("browse: mesh %v, %d SLOs", b.HasMesh(), len(b.SLOs))
	}
	files, err := Render(model)
	if err != nil {
		t.Fatal(err)
	}
	mesh := string(fileByPath(t, files, pathMeshRules))
	if strings.Contains(mesh, "cuj: browse") {
		t.Error("browse still has mesh composite rules")
	}
}

func TestCatalog(t *testing.T) {
	c := string(fileByPath(t, fixtureFiles(t), pathCatalog))
	for _, want := range []string{
		"| fast | 13.44 | 1h | 5m | 2 % | page | ticket |",
		"| slow | 2.8 | 24h | 2h | 10 % | ticket | ticket |",
		"| fast | 6.72 | 1h | 5m | 1 % | page | ticket |",
		"| slow | 1.4 | 24h | 2h | 5 % | ticket | ticket |",
		"- medium: burn rate > 5.6 over 6h and 30m (ticket)",
		"201 failed runs (at one run a minute)",
		"403 bad 1m windows",
		"(left out, plain TCP: redis-cart)",
		"(left out, entry covered by the prober: frontend)",
		"`CUJ checkout: mesh latency medium burn` (ticket, burn rate > 2.8)",
		"`CUJ checkout: prober latency medium burn` (page, burn rate > 2.8)",
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
