package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// Inputs names the files a generation run read, as given on the command line
// (relative to the repository root). They appear in every generated header.
type Inputs struct {
	SLOs   string
	CUJMap string
}

// Model is the spec and the CUJ map joined: everything the renderers need.
type Model struct {
	Spec     *Spec
	Map      *CUJMap
	Inputs   Inputs
	Journeys []Journey
	// Thresholds are the distinct latency thresholds in milliseconds, ascending.
	Thresholds []int
}

// Journey is one CUJ with its targets, its critical services and its SLOs.
type Journey struct {
	JourneySpec
	// Critical is the CUJ map's list, sorted.
	Critical []string
	// MeshServices is the critical services minus excluded and TCP-only ones.
	MeshServices []string
	// Excluded are critical services left out on purpose (mesh.excludedServices):
	// the entry (frontend), whose ratio mixes every journey's routes. The prober
	// covers it.
	Excluded []string
	// TCPSkipped are critical services without request metrics (plain TCP).
	TCPSkipped  []string
	ProberSteps []string
	SLOs        []SLO
}

// SLO is one objective of one journey on one signal.
type SLO struct {
	Signal string // prober, mesh or synthetics
	SLI    string // availability or latency
	Target float64
}

const (
	sliAvailability = "availability"
	sliLatency      = "latency"
)

// ID is the Cloud Monitoring SLO id, e.g. "prober-availability".
func (s SLO) ID() string { return s.Signal + "-" + s.SLI }

// BuildModel joins the spec with the CUJ map. Journeys in the map without an
// SLO are returned as warnings; journeys with an SLO but missing from the map
// are errors, since their composite would be empty.
func BuildModel(spec *Spec, m *CUJMap, in Inputs) (*Model, []string, error) {
	byID := map[string]CUJMapJourney{}
	for _, j := range m.Journeys {
		byID[j.ID] = j
	}
	model := &Model{Spec: spec, Map: m, Inputs: in}
	var missing []string
	warnings := spec.Warnings()
	thresholds := map[int]bool{}
	for _, js := range spec.Journeys {
		mj, ok := byID[js.ID]
		if !ok {
			missing = append(missing, js.ID)
			continue
		}
		j := Journey{
			JourneySpec: js,
			Critical:    sortedUnique(mj.Critical),
			ProberSteps: spec.Prober.LatencySteps[js.ID],
		}
		for _, svc := range j.Critical {
			switch {
			case slices.Contains(spec.Mesh.ExcludedServices, svc):
				j.Excluded = append(j.Excluded, svc)
			case slices.Contains(spec.Mesh.TCPServices, svc):
				j.TCPSkipped = append(j.TCPSkipped, svc)
			default:
				j.MeshServices = append(j.MeshServices, svc)
			}
		}
		signals := []string{signalProber, signalMesh}
		if len(j.MeshServices) == 0 {
			signals = signals[:1]
			warnings = append(warnings, fmt.Sprintf("journey %q has no critical service with request metrics; it gets prober SLOs only", js.ID))
		}
		for _, sig := range signals {
			j.SLOs = append(j.SLOs,
				SLO{Signal: sig, SLI: sliAvailability, Target: js.Availability.Target},
				SLO{Signal: sig, SLI: sliLatency, Target: js.Latency.Target})
		}
		thresholds[js.Latency.ThresholdMs] = true
		model.Journeys = append(model.Journeys, j)
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("journeys in the SLO spec but not in the CUJ map: %s", strings.Join(missing, ", "))
	}
	for _, mj := range m.Journeys {
		if !slices.ContainsFunc(spec.Journeys, func(js JourneySpec) bool { return js.ID == mj.ID }) {
			warnings = append(warnings, fmt.Sprintf("journey %q is in the CUJ map but has no SLO in the spec; skipped", mj.ID))
		}
	}
	for t := range thresholds {
		model.Thresholds = append(model.Thresholds, t)
	}
	sort.Ints(model.Thresholds)
	return model, warnings, nil
}

// BurnRate is the burn-rate threshold of a tier: the rate at which the tier's
// share of the budget is spent within its long window. 2 % of a 28-day budget
// in one hour is 0.02 x 672 h / 1 h = 13.44.
func BurnRate(t TierSpec, window Duration) float64 {
	if t.LongWindow.D() <= 0 {
		return 0
	}
	return round4(t.BudgetConsumed * float64(window.D()) / float64(t.LongWindow.D()))
}

// TiersFor returns the alert tiers of an SLI.
func (m *Model) TiersFor(sli string) []TierSpec {
	if sli == sliLatency {
		return m.Spec.Alerting.LatencyTiers
	}
	return m.Spec.Alerting.AvailabilityTiers
}

// Severity is page only for a page tier of a signal in alerting.pageSignals;
// everything else opens a ticket.
func (m *Model) Severity(signal string, t TierSpec) string {
	if t.Severity == severityPage && slices.Contains(m.Spec.Alerting.PageSignals, signal) {
		return severityPage
	}
	return severityTicket
}

// HasMesh reports whether a journey has mesh SLOs.
func (j Journey) HasMesh() bool { return len(j.MeshServices) > 0 }

// RunbookURL is the journey's runbook (docs/runbooks/cuj-<id>.md).
func (m *Model) RunbookURL(journey string) string {
	return m.Spec.Alerting.RunbookBaseURL + "/cuj-" + journey + ".md"
}

// HeaderLines is the "generated" notice written at the top of every output file.
func (m *Model) HeaderLines() []string {
	lines := []string{fmt.Sprintf("Code generated by slo-gen from %s and %s; DO NOT EDIT.", m.Inputs.SLOs, m.Inputs.CUJMap)}
	second := "Regenerate with `make slo-generate`."
	if md := m.Map.Metadata; md.GeneratedAt != "" || md.Generator != "" {
		second = fmt.Sprintf("CUJ map generated %s by %s. ", orUnknown(md.GeneratedAt), orUnknown(md.Generator)) + second
	}
	return append(lines, second)
}

// Header is HeaderLines on one line.
func (m *Model) Header() string { return strings.Join(m.HeaderLines(), " ") }

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// snake turns a journey id into a Terraform-friendly name part.
func snake(s string) string { return strings.ReplaceAll(s, "-", "_") }

// tfName builds a Terraform resource name with the area prefix "slo_".
func tfName(parts ...string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = snake(p)
	}
	return "slo_" + strings.Join(out, "_")
}

func sortedUnique(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// percent formats a fraction as a percentage without float noise: 0.995 -> "99.5".
func percent(v float64) string { return formatFloat(round4(v * 100)) }

// budgetShare is the share of the window's error budget that one tier spends.
func budgetShare(t TierSpec) string { return percent(t.BudgetConsumed) + " %" }
