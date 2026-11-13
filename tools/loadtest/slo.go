package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// sloSpec is the part of slo/slos.yaml (kind SLOSpec, owned by the SLO generator) this tool reads.
// Unknown keys are ignored: the generator may add fields this tool does not need.
type sloSpec struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Window     string       `yaml:"window"`
	Journeys   []journeySLO `yaml:"journeys"`
}

type journeySLO struct {
	ID           string          `yaml:"id" json:"id"`
	Availability availabilitySLO `yaml:"availability" json:"availability"`
	Latency      latencySLO      `yaml:"latency" json:"latency"`
}

type availabilitySLO struct {
	Target float64 `yaml:"target" json:"target"`
}

type latencySLO struct {
	ThresholdMs float64 `yaml:"thresholdMs" json:"thresholdMs"`
	Target      float64 `yaml:"target" json:"target"`
}

var journeyID = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func loadSLOs(path string) (sloSpec, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return sloSpec{}, err
	}
	var spec sloSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return sloSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := spec.validate(); err != nil {
		return sloSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

func (s sloSpec) validate() error {
	var errs []error
	if s.APIVersion != "" && s.APIVersion != "mamori/v1" {
		errs = append(errs, fmt.Errorf("apiVersion is %q, want mamori/v1", s.APIVersion))
	}
	if s.Kind != "SLOSpec" {
		errs = append(errs, fmt.Errorf("kind is %q, want SLOSpec", s.Kind))
	}
	if len(s.Journeys) == 0 {
		errs = append(errs, errors.New("no journeys"))
	}
	seen := map[string]bool{}
	for i, j := range s.Journeys {
		name := j.ID
		if !journeyID.MatchString(j.ID) {
			name = fmt.Sprintf("journeys[%d]", i)
			errs = append(errs, fmt.Errorf("%s: id %q is not a CUJ id", name, j.ID))
		}
		if seen[j.ID] {
			errs = append(errs, fmt.Errorf("%s: listed twice", name))
		}
		seen[j.ID] = true
		if !(j.Availability.Target > 0 && j.Availability.Target < 1) {
			errs = append(errs, fmt.Errorf("%s: availability target %g must be between 0 and 1", name, j.Availability.Target))
		}
		if !(j.Latency.Target > 0 && j.Latency.Target < 1) {
			errs = append(errs, fmt.Errorf("%s: latency target %g must be between 0 and 1", name, j.Latency.Target))
		}
		if !(j.Latency.ThresholdMs > 0) {
			errs = append(errs, fmt.Errorf("%s: latency thresholdMs %g must be above 0", name, j.Latency.ThresholdMs))
		}
	}
	return errors.Join(errs...)
}

// thresholdLabel is how k6 writes a threshold into the threshold_ms label (JavaScript's String()).
func thresholdLabel(ms float64) string { return strconv.FormatFloat(ms, 'f', -1, 64) }
