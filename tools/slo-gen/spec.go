package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Spec is slo/slos.yaml: targets per journey, and how the SLIs are measured
// and alerted on. Fields are explained in that file.
type Spec struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Window     Duration      `yaml:"window"`
	Journeys   []JourneySpec `yaml:"journeys"`
	GCP        GCPSpec       `yaml:"gcp"`
	Prober     ProberSpec    `yaml:"prober"`
	Mesh       MeshSpec      `yaml:"mesh"`
	Alerting   AlertingSpec  `yaml:"alerting"`
	Datadog    DatadogSpec   `yaml:"datadog"`
}

// JourneySpec holds one journey's targets.
type JourneySpec struct {
	ID           string           `yaml:"id"`
	Availability AvailabilitySpec `yaml:"availability"`
	Latency      LatencySpec      `yaml:"latency"`
}

// AvailabilitySpec is the share of good events the journey must reach.
type AvailabilitySpec struct {
	Target float64 `yaml:"target"`
}

// LatencySpec is the share of requests that must be faster than ThresholdMs.
type LatencySpec struct {
	ThresholdMs int     `yaml:"thresholdMs"`
	Target      float64 `yaml:"target"`
}

// GCPSpec holds Google Cloud settings.
type GCPSpec struct {
	Cluster      string   `yaml:"cluster"`
	RuleInterval Duration `yaml:"ruleInterval"`
}

// ProberSpec describes the black-box signal.
type ProberSpec struct {
	Namespace     string              `yaml:"namespace"`
	LatencySteps  map[string][]string `yaml:"latencySteps"`
	RecordWindows []Duration          `yaml:"recordWindows"`
}

// MeshSpec describes the Istio composite signal.
type MeshSpec struct {
	Namespace     string     `yaml:"namespace"`
	EntryServices []string   `yaml:"entryServices"`
	TCPServices   []string   `yaml:"tcpServices"`
	WindowPeriod  Duration   `yaml:"windowPeriod"`
	SLORateWindow Duration   `yaml:"sloRateWindow"`
	RecordWindows []Duration `yaml:"recordWindows"`
}

// AlertingSpec holds the burn-rate tiers and alert settings.
type AlertingSpec struct {
	Tiers          []TierSpec `yaml:"tiers"`
	NoDataAfter    Duration   `yaml:"noDataAfter"`
	AutoClose      Duration   `yaml:"autoClose"`
	RunbookBaseURL string     `yaml:"runbookBaseURL"`
}

// TierSpec is one multiwindow burn-rate alert tier.
type TierSpec struct {
	Name           string   `yaml:"name"`
	Severity       string   `yaml:"severity"`
	BudgetConsumed float64  `yaml:"budgetConsumed"`
	LongWindow     Duration `yaml:"longWindow"`
	ShortWindow    Duration `yaml:"shortWindow"`
	Signals        []string `yaml:"signals"`
}

// DatadogSpec holds the settings of the Datadog output.
type DatadogSpec struct {
	Timeframes []string         `yaml:"timeframes"`
	BurnRate   DatadogBurnRates `yaml:"burnRate"`
}

// DatadogBurnRates configures the metric-based SLOs that carry Datadog's
// burn-rate monitors.
type DatadogBurnRates struct {
	Enabled    bool   `yaml:"enabled"`
	Metric     string `yaml:"metric"`
	TestTag    string `yaml:"testTag"`
	GoodFilter string `yaml:"goodFilter"`
}

// CUJMap is slo/cuj-map.yaml, written by tools/cuj-discovery. Unknown fields
// are allowed so that the discovery tool can add some without breaking this one.
type CUJMap struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   CUJMapMetadata  `yaml:"metadata"`
	Journeys   []CUJMapJourney `yaml:"journeys"`
}

// CUJMapMetadata says how and when the map was made.
type CUJMapMetadata struct {
	GeneratedAt      string  `yaml:"generatedAt"`
	Generator        string  `yaml:"generator"`
	OnlineBoutique   string  `yaml:"onlineBoutique"`
	RunsPerCase      int     `yaml:"runsPerCase"`
	FailureThreshold float64 `yaml:"failureThreshold"`
	Fault            string  `yaml:"fault"`
}

// CUJMapJourney lists the services whose failure breaks a journey.
type CUJMapJourney struct {
	ID          string                  `yaml:"id"`
	Critical    []string                `yaml:"critical"`
	NonCritical []string                `yaml:"nonCritical"`
	Results     map[string]CUJMapResult `yaml:"results"`
}

// CUJMapResult is the journey's failure rate while one service was broken.
type CUJMapResult struct {
	FailureRate float64 `yaml:"failureRate"`
	Runs        int     `yaml:"runs"`
}

// Duration is a whole number of seconds, minutes, hours or days ("28d").
type Duration time.Duration

var durationPattern = regexp.MustCompile(`^([1-9][0-9]*)(s|m|h|d)$`)

// ParseDuration parses "30s", "5m", "1h" or "28d".
func ParseDuration(s string) (Duration, error) {
	m := durationPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid duration %q: want a whole number and one of s, m, h, d (e.g. 5m, 28d)", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	return Duration(time.Duration(n) * unit), nil
}

// UnmarshalYAML reads a duration written as "5m".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String formats the duration with the largest unit that divides it evenly,
// which is also the format PromQL range selectors take ("5m", "1h", "28d").
// Days only above 48 hours, so alert windows read as hours ("24h"), the form
// Datadog's burn-rate windows take (1h to 48h).
func (d Duration) String() string {
	td := time.Duration(d)
	for _, u := range []struct {
		suffix string
		size   time.Duration
	}{{"d", 24 * time.Hour}, {"h", time.Hour}, {"m", time.Minute}} {
		if u.suffix == "d" && td <= 48*time.Hour {
			continue
		}
		if td >= u.size && td%u.size == 0 {
			return strconv.FormatInt(int64(td/u.size), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(td/time.Second), 10) + "s"
}

// Seconds formats the duration as Google APIs want it ("3600s").
func (d Duration) Seconds() string {
	return strconv.FormatInt(int64(time.Duration(d)/time.Second), 10) + "s"
}

// LoadSpec reads and validates slo/slos.yaml. Unknown fields are errors, so a
// typo cannot silently fall back to a default.
func LoadSpec(path string) (*Spec, error) {
	var s Spec
	if err := decodeFile(path, &s, true); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// LoadCUJMap reads and validates the CUJ map.
func LoadCUJMap(path string) (*CUJMap, error) {
	var m CUJMap
	if err := decodeFile(path, &m, false); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func decodeFile(path string, out any, strict bool) error {
	b, err := os.ReadFile(path) //nolint:gosec // the path is a command-line argument by design
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(strict)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: file is empty", path)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

var (
	idPattern       = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	tierNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	knownSignals    = []string{signalProber, signalMesh, signalSynthetics}
	datadogFrames   = []string{"7d", "30d", "90d"}
)

const (
	signalProber     = "prober"
	signalMesh       = "mesh"
	signalSynthetics = "synthetics"

	severityPage   = "page"
	severityTicket = "ticket"

	// Cloud Monitoring burn-rate alerts look back at most 24 hours.
	maxCloudMonitoringLookback = 24 * time.Hour
	// Datadog burn-rate monitors take a long window of 1 to 48 hours.
	minDatadogLongWindow = time.Hour
)

// Validate checks the spec and returns every problem found, not only the first.
func (s *Spec) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if s.APIVersion != "mamori/v1" || s.Kind != "SLOSpec" {
		add("apiVersion/kind must be mamori/v1 SLOSpec, got %q %q", s.APIVersion, s.Kind)
	}
	w := s.Window.D()
	if w%(24*time.Hour) != 0 || w < 24*time.Hour || w > 30*24*time.Hour {
		add("window must be whole days from 1d to 30d (Cloud Monitoring's rolling periods), got %s", s.Window)
	}

	if len(s.Journeys) == 0 {
		add("journeys: at least one journey is needed")
	}
	seen := map[string]bool{}
	for i, j := range s.Journeys {
		where := fmt.Sprintf("journeys[%d] (%s)", i, j.ID)
		if !idPattern.MatchString(j.ID) {
			add("%s: id must be lowercase words joined by dashes", where)
		}
		if seen[j.ID] {
			add("%s: duplicate id", where)
		}
		seen[j.ID] = true
		if !isFraction(j.Availability.Target) {
			add("%s: availability.target must be between 0 and 1 (exclusive), got %v", where, j.Availability.Target)
		}
		if !isFraction(j.Latency.Target) {
			add("%s: latency.target must be between 0 and 1 (exclusive), got %v", where, j.Latency.Target)
		}
		if j.Latency.ThresholdMs <= 0 {
			add("%s: latency.thresholdMs must be positive, got %d", where, j.Latency.ThresholdMs)
		}
		steps := s.Prober.LatencySteps[j.ID]
		if len(steps) == 0 {
			add("prober.latencySteps: no steps for journey %q", j.ID)
		}
		for _, st := range steps {
			if !idPattern.MatchString(st) {
				add("prober.latencySteps.%s: invalid step name %q", j.ID, st)
			}
		}
	}
	for id := range s.Prober.LatencySteps {
		if !seen[id] {
			add("prober.latencySteps: journey %q is not in journeys", id)
		}
	}

	if s.GCP.Cluster == "" {
		add("gcp.cluster is required")
	}
	if s.GCP.RuleInterval.D() < 10*time.Second {
		add("gcp.ruleInterval must be at least 10s, got %s", s.GCP.RuleInterval)
	}
	if s.Prober.Namespace == "" {
		add("prober.namespace is required")
	}
	validWindows(&errs, "prober.recordWindows", s.Prober.RecordWindows)

	m := s.Mesh
	if m.Namespace == "" {
		add("mesh.namespace is required")
	}
	for _, svc := range append(append([]string{}, m.EntryServices...), m.TCPServices...) {
		if !idPattern.MatchString(svc) {
			add("mesh: invalid service name %q", svc)
		}
	}
	if p := m.WindowPeriod.D(); p < time.Minute || p > 24*time.Hour || p%time.Minute != 0 {
		add("mesh.windowPeriod must be whole minutes from 1m to 24h (Cloud Monitoring), got %s", m.WindowPeriod)
	}
	if m.SLORateWindow.D() < time.Minute {
		add("mesh.sloRateWindow must be at least 1m, got %s", m.SLORateWindow)
	}
	validWindows(&errs, "mesh.recordWindows", m.RecordWindows)

	s.validateAlerting(&errs)
	s.validateDatadog(&errs)
	return errors.Join(errs...)
}

func (s *Spec) validateAlerting(errs *[]error) {
	add := func(format string, a ...any) { *errs = append(*errs, fmt.Errorf(format, a...)) }
	a := s.Alerting
	if len(a.Tiers) == 0 {
		add("alerting.tiers: at least one tier is needed")
	}
	names := map[string]bool{}
	for i, t := range a.Tiers {
		where := fmt.Sprintf("alerting.tiers[%d] (%s)", i, t.Name)
		if !tierNamePattern.MatchString(t.Name) || names[t.Name] {
			add("%s: name must be a unique lowercase word", where)
		}
		names[t.Name] = true
		if t.Severity != severityPage && t.Severity != severityTicket {
			add("%s: severity must be page or ticket, got %q", where, t.Severity)
		}
		if t.BudgetConsumed <= 0 || t.BudgetConsumed > 1 {
			add("%s: budgetConsumed must be in (0, 1], got %v", where, t.BudgetConsumed)
		}
		if t.LongWindow.D() > maxCloudMonitoringLookback || t.LongWindow.D() < minDatadogLongWindow {
			add("%s: longWindow must be from 1h (Datadog) to 24h (Cloud Monitoring), got %s", where, t.LongWindow)
		}
		if t.ShortWindow.D() < time.Minute || t.ShortWindow.D() >= t.LongWindow.D() {
			add("%s: shortWindow must be at least 1m and shorter than longWindow, got %s", where, t.ShortWindow)
		}
		if len(t.Signals) == 0 {
			add("%s: signals must name at least one of %s", where, strings.Join(knownSignals, ", "))
		}
		for _, sig := range t.Signals {
			if !slices.Contains(knownSignals, sig) {
				add("%s: unknown signal %q (want %s)", where, sig, strings.Join(knownSignals, ", "))
			}
		}
		if t.LongWindow.D() <= 0 {
			continue
		}
		// A burn rate above 1/(1-target) needs more than 100 % errors: the alert could never fire.
		burn := BurnRate(t, s.Window)
		for _, j := range s.Journeys {
			for _, target := range []float64{j.Availability.Target, j.Latency.Target} {
				if isFraction(target) && burn > 1/(1-target) {
					add("%s: burn rate %v can never be reached by journey %q with target %v", where, burn, j.ID, target)
				}
			}
		}
	}
	if a.NoDataAfter.D() < 2*time.Minute {
		add("alerting.noDataAfter must be at least 2m, got %s", a.NoDataAfter)
	}
	if a.AutoClose.D() < 30*time.Minute {
		add("alerting.autoClose must be at least 30m, got %s", a.AutoClose)
	}
	if !strings.HasPrefix(a.RunbookBaseURL, "https://") || strings.HasSuffix(a.RunbookBaseURL, "/") {
		add("alerting.runbookBaseURL must be an https URL without a trailing slash, got %q", a.RunbookBaseURL)
	}
}

func (s *Spec) validateDatadog(errs *[]error) {
	add := func(format string, a ...any) { *errs = append(*errs, fmt.Errorf(format, a...)) }
	d := s.Datadog
	if len(d.Timeframes) == 0 {
		add("datadog.timeframes: at least one of %s is needed", strings.Join(datadogFrames, ", "))
	}
	for _, f := range d.Timeframes {
		if !slices.Contains(datadogFrames, f) {
			add("datadog.timeframes: %q is not one of %s", f, strings.Join(datadogFrames, ", "))
		}
	}
	if d.BurnRate.Enabled && (d.BurnRate.Metric == "" || d.BurnRate.TestTag == "" || d.BurnRate.GoodFilter == "") {
		add("datadog.burnRate: metric, testTag and goodFilter are required when enabled")
	}
}

func validWindows(errs *[]error, field string, ws []Duration) {
	if len(ws) == 0 {
		*errs = append(*errs, fmt.Errorf("%s: at least one window is needed", field))
	}
	for _, w := range ws {
		if w.D() < time.Minute {
			*errs = append(*errs, fmt.Errorf("%s: windows must be at least 1m, got %s", field, w))
		}
	}
}

// Validate checks the CUJ map.
func (m *CUJMap) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if m.APIVersion != "mamori/v1" || m.Kind != "CUJMap" {
		add("apiVersion/kind must be mamori/v1 CUJMap, got %q %q", m.APIVersion, m.Kind)
	}
	seen := map[string]bool{}
	for i, j := range m.Journeys {
		where := fmt.Sprintf("journeys[%d] (%s)", i, j.ID)
		if !idPattern.MatchString(j.ID) {
			add("%s: invalid id", where)
		}
		if seen[j.ID] {
			add("%s: duplicate id", where)
		}
		seen[j.ID] = true
		for _, svc := range append(append([]string{}, j.Critical...), j.NonCritical...) {
			if !idPattern.MatchString(svc) {
				add("%s: invalid service name %q", where, svc)
			}
		}
		for _, svc := range j.Critical {
			if slices.Contains(j.NonCritical, svc) {
				add("%s: %s is both critical and non-critical", where, svc)
			}
		}
	}
	return errors.Join(errs...)
}

func isFraction(v float64) bool { return v > 0 && v < 1 }
