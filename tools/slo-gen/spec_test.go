package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30s": 30 * time.Second, "5m": 5 * time.Minute, "1h": time.Hour, "28d": 28 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil || got.D() != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got.D(), err, want)
		}
	}
	for _, bad := range []string{"", "5", "1.5h", "0m", "-1h", "1w", "05m"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q): want an error", bad)
		}
	}
}

func TestDurationFormats(t *testing.T) {
	cases := []struct {
		d       time.Duration
		str     string
		seconds string
	}{
		{28 * 24 * time.Hour, "28d", "2419200s"},
		{24 * time.Hour, "24h", "86400s"},
		{72 * time.Hour, "3d", "259200s"},
		{6 * time.Hour, "6h", "21600s"},
		{90 * time.Minute, "90m", "5400s"},
		{2 * time.Minute, "2m", "120s"},
		{45 * time.Second, "45s", "45s"},
	}
	for _, c := range cases {
		d := Duration(c.d)
		if d.String() != c.str || d.Seconds() != c.seconds {
			t.Errorf("%v: got %s / %s, want %s / %s", c.d, d.String(), d.Seconds(), c.str, c.seconds)
		}
	}
}

func TestBurnRate(t *testing.T) {
	window := Duration(28 * 24 * time.Hour)
	cases := []struct {
		budget float64
		long   time.Duration
		want   float64
	}{
		{0.02, time.Hour, 13.44},
		{0.05, 6 * time.Hour, 5.6},
		{0.10, 24 * time.Hour, 2.8},
	}
	for _, c := range cases {
		got := BurnRate(TierSpec{BudgetConsumed: c.budget, LongWindow: Duration(c.long)}, window)
		if got != c.want {
			t.Errorf("BurnRate(%v over %v) = %v, want %v", c.budget, c.long, got, c.want)
		}
	}
	if got := BurnRate(TierSpec{BudgetConsumed: 0.1}, window); got != 0 {
		t.Errorf("BurnRate without a long window = %v, want 0", got)
	}
}

func TestLeMatch(t *testing.T) {
	cases := []struct {
		bound       float64
		match, miss []string
	}{
		{0.5, []string{"0.5", "0.50"}, []string{"0.55", "5", "0.05"}},
		{1, []string{"1", "1.0", "1.00"}, []string{"10", "1.5", "0.1"}},
		{500, []string{"500", "500.0"}, []string{"5000", "50"}},
	}
	for _, c := range cases {
		re := regexp.MustCompile("^(?:" + leMatch(c.bound) + ")$")
		for _, s := range c.match {
			if !re.MatchString(s) {
				t.Errorf("leMatch(%v) should match %q", c.bound, s)
			}
		}
		for _, s := range c.miss {
			if re.MatchString(s) {
				t.Errorf("leMatch(%v) should not match %q", c.bound, s)
			}
		}
	}
}

func loadFixtureSpec(t *testing.T) *Spec {
	t.Helper()
	s, err := LoadSpec(filepath.Join(fixtureRoot, "slo/slos.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSpecValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Spec)
		want   string
	}{
		"kind":            {func(s *Spec) { s.Kind = "SLO" }, "apiVersion/kind"},
		"window days":     {func(s *Spec) { s.Window = Duration(36 * time.Hour) }, "window must be whole days"},
		"window too long": {func(s *Spec) { s.Window = Duration(31 * 24 * time.Hour) }, "window must be whole days"},
		"no journeys":     {func(s *Spec) { s.Journeys = nil }, "at least one journey"},
		"bad id":          {func(s *Spec) { s.Journeys[0].ID = "Browse" }, "id must be lowercase"},
		"duplicate id":    {func(s *Spec) { s.Journeys[1].ID = "browse" }, "duplicate id"},
		"target 1":        {func(s *Spec) { s.Journeys[0].Availability.Target = 1 }, "availability.target"},
		"latency target":  {func(s *Spec) { s.Journeys[0].Latency.Target = 0 }, "latency.target"},
		"threshold":       {func(s *Spec) { s.Journeys[0].Latency.ThresholdMs = 0 }, "thresholdMs"},
		"no steps":        {func(s *Spec) { delete(s.Prober.LatencySteps, "browse") }, `no steps for journey "browse"`},
		"bad step":        {func(s *Spec) { s.Prober.LatencySteps["browse"] = []string{"Home"} }, "invalid step name"},
		"unknown steps":   {func(s *Spec) { s.Prober.LatencySteps["nope"] = []string{"x"} }, `journey "nope" is not in journeys`},
		"cluster":         {func(s *Spec) { s.GCP.Cluster = "" }, "gcp.cluster"},
		"rule interval":   {func(s *Spec) { s.GCP.RuleInterval = Duration(time.Second) }, "ruleInterval"},
		"prober ns":       {func(s *Spec) { s.Prober.Namespace = "" }, "prober.namespace"},
		"prober windows":  {func(s *Spec) { s.Prober.RecordWindows = nil }, "prober.recordWindows"},
		"record window":   {func(s *Spec) { s.Mesh.RecordWindows = []Duration{Duration(time.Second)} }, "at least 1m"},
		"mesh ns":         {func(s *Spec) { s.Mesh.Namespace = "" }, "mesh.namespace"},
		"service name":    {func(s *Spec) { s.Mesh.TCPServices = []string{"Redis Cart"} }, "invalid service name"},
		"excluded name":   {func(s *Spec) { s.Mesh.ExcludedServices = []string{"Front End"} }, "invalid service name"},
		"grpc no ok":      {func(s *Spec) { s.Mesh.GoodGRPCStatuses = []string{"5"} }, "must include 0"},
		"grpc code":       {func(s *Spec) { s.Mesh.GoodGRPCStatuses = []string{"0", "17", "05"} }, `"17" is not a gRPC status`},
		"window period":   {func(s *Spec) { s.Mesh.WindowPeriod = Duration(90 * time.Second) }, "mesh.windowPeriod"},
		"slo rate window": {func(s *Spec) { s.Mesh.SLORateWindow = 0 }, "mesh.sloRateWindow"},
		"no tiers":        {func(s *Spec) { s.Alerting.AvailabilityTiers = nil }, "availabilityTiers: at least one tier"},
		"no latency":      {func(s *Spec) { s.Alerting.LatencyTiers = nil }, "latencyTiers: at least one tier"},
		"tier name":       {func(s *Spec) { s.Alerting.AvailabilityTiers[1].Name = "fast" }, "unique lowercase word"},
		"severity":        {func(s *Spec) { s.Alerting.LatencyTiers[0].Severity = "urgent" }, "page or ticket"},
		"budget":          {func(s *Spec) { s.Alerting.AvailabilityTiers[0].BudgetConsumed = 0 }, "budgetConsumed"},
		"lookback":        {func(s *Spec) { s.Alerting.AvailabilityTiers[2].LongWindow = Duration(72 * time.Hour) }, "longWindow must be from 1h"},
		"short >= long":   {func(s *Spec) { s.Alerting.AvailabilityTiers[0].ShortWindow = Duration(time.Hour) }, "shorter than longWindow"},
		"unknown signal":  {func(s *Spec) { s.Alerting.PageSignals = []string{"logs"} }, `unknown signal "logs"`},
		"unreachable": {func(s *Spec) {
			s.Alerting.LatencyTiers[0].BudgetConsumed = 0.05 // 33.6 > 1/(1-0.95)
		}, "can never be reached"},
		"no data":        {func(s *Spec) { s.Alerting.NoDataAfter = Duration(time.Minute) }, "noDataAfter"},
		"auto close":     {func(s *Spec) { s.Alerting.AutoClose = Duration(time.Minute) }, "autoClose"},
		"runbook url":    {func(s *Spec) { s.Alerting.RunbookBaseURL = "http://x/" }, "runbookBaseURL"},
		"timeframes":     {func(s *Spec) { s.Datadog.Timeframes = nil }, "datadog.timeframes"},
		"bad timeframe":  {func(s *Spec) { s.Datadog.Timeframes = []string{"28d"} }, `"28d" is not one of`},
		"burn rate tags": {func(s *Spec) { s.Datadog.BurnRate.TestTag = "" }, "datadog.burnRate"},
		"datadog tiers":  {func(s *Spec) { s.Datadog.Tiers = nil }, "datadog.tiers: at least one"},
		"datadog tier":   {func(s *Spec) { s.Datadog.Tiers = []string{"rapid"} }, `"rapid" is not one of`},
		"short window":   {func(s *Spec) { s.Datadog.MinShortWindow = 0 }, "datadog.minShortWindow"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := loadFixtureSpec(t)
			c.mutate(s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestSpecWarnings(t *testing.T) {
	s := loadFixtureSpec(t)
	if w := s.Warnings(); len(w) != 0 {
		t.Errorf("the fixture spec should have no warnings, got %v", w)
	}
	// 2 % of a 95 % budget in 1 h is a burn rate of 13.44: over half the maximum of 20.
	s.Alerting.LatencyTiers[0].BudgetConsumed = 0.02
	w := s.Warnings()
	if len(w) != 4 || !strings.Contains(w[0], "more than half the maximum 20") || !strings.Contains(w[0], "over 67.2 % of events are bad") {
		t.Errorf("warnings = %v", w)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("a warning is not an error: %v", err)
	}
}

func TestLoadSpecErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]struct{ content, want string }{
		"unknown field": {"apiVersion: mamori/v1\nkind: SLOSpec\nwindoww: 28d\n", "field windoww not found"},
		"bad duration":  {"window: 4w\n", "invalid duration"},
		"bad type":      {"window: [1]\n", "cannot unmarshal"},
		"empty":         {"", "file is empty"},
		"invalid":       {"apiVersion: mamori/v1\nkind: SLOSpec\nwindow: 28d\n", "at least one journey"},
	}
	for name, c := range cases {
		if _, err := LoadSpec(write(name+".yaml", c.content)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
	if _, err := LoadSpec(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file: want an error")
	}
}

func TestCUJMapValidation(t *testing.T) {
	cases := map[string]struct {
		m    CUJMap
		want string
	}{
		"kind": {CUJMap{APIVersion: "mamori/v1", Kind: "Map"}, "apiVersion/kind"},
		"id": {CUJMap{APIVersion: "mamori/v1", Kind: "CUJMap", Journeys: []CUJMapJourney{
			{ID: "Browse"}}}, "invalid id"},
		"duplicate": {CUJMap{APIVersion: "mamori/v1", Kind: "CUJMap", Journeys: []CUJMapJourney{
			{ID: "browse"}, {ID: "browse"}}}, "duplicate id"},
		"service": {CUJMap{APIVersion: "mamori/v1", Kind: "CUJMap", Journeys: []CUJMapJourney{
			{ID: "browse", Critical: []string{"Cart Service"}}}}, "invalid service name"},
		"both": {CUJMap{APIVersion: "mamori/v1", Kind: "CUJMap", Journeys: []CUJMapJourney{
			{ID: "browse", Critical: []string{"adservice"}, NonCritical: []string{"adservice"}}}}, "both critical and non-critical"},
	}
	for name, c := range cases {
		if err := c.m.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
	if _, err := LoadCUJMap(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Error("missing map: want an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCUJMap(bad); err == nil || !strings.Contains(err.Error(), "apiVersion/kind") {
		t.Errorf("invalid map: got %v", err)
	}
}

func TestBuildModel(t *testing.T) {
	spec := loadFixtureSpec(t)
	m, err := LoadCUJMap(filepath.Join(fixtureRoot, "slo/cuj-map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	model, warnings, err := BuildModel(spec, m, fixtureInputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"wishlist"`) {
		t.Errorf("warnings = %v, want one about wishlist", warnings)
	}
	if len(model.Journeys) != 4 || len(model.Journeys[0].SLOs) != 4 {
		t.Errorf("got %d journeys with %d SLOs", len(model.Journeys), len(model.Journeys[0].SLOs))
	}
	if strings.Join(intsToStrings(model.Thresholds), ",") != "500,1000" {
		t.Errorf("thresholds = %v", model.Thresholds)
	}

	m.Journeys = m.Journeys[1:] // drop browse
	if _, _, err := BuildModel(spec, m, fixtureInputs); err == nil || !strings.Contains(err.Error(), "browse") {
		t.Errorf("missing journey: got %v", err)
	}
}

func intsToStrings(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = formatFloat(float64(v))
	}
	return out
}

func TestHeaderWithoutMetadata(t *testing.T) {
	m := &Model{Map: &CUJMap{}, Inputs: fixtureInputs}
	if h := m.Header(); strings.Contains(h, "CUJ map generated") || !strings.Contains(h, "DO NOT EDIT") {
		t.Errorf("header = %q", h)
	}
	m.Map.Metadata.Generator = "x"
	if h := m.Header(); !strings.Contains(h, "generated (unknown) by x") {
		t.Errorf("header = %q", h)
	}
}
