package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"time"

	"go.yaml.in/yaml/v3"
)

//go:embed config.yaml
var defaultConfigYAML []byte

// config holds the settings that stay the same from run to run (config.yaml).
type config struct {
	Cluster               string         `yaml:"cluster" json:"cluster"`
	Warmup                duration       `yaml:"warmup" json:"warmup"`
	MinDeliveredRatio     float64        `yaml:"minDeliveredRatio" json:"minDeliveredRatio"`
	MinRequestsPerJourney float64        `yaml:"minRequestsPerJourney" json:"minRequestsPerJourney"`
	GeneratorCPULimit     float64        `yaml:"generatorCPULimit" json:"generatorCPULimit"`
	RateLimitShare        float64        `yaml:"rateLimitShare" json:"rateLimitShare"`
	Metrics               metricNames    `yaml:"metrics" json:"metrics"`
	Context               contextQueries `yaml:"context" json:"context"`
	Cost                  costModel      `yaml:"cost" json:"cost"`
}

type metricNames struct {
	Requests       string `yaml:"requests" json:"requests"`
	Good           string `yaml:"good" json:"good"`
	Fast           string `yaml:"fast" json:"fast"`
	Dropped        string `yaml:"dropped" json:"dropped"`
	Duration       string `yaml:"duration" json:"duration"`
	HTTPRequests   string `yaml:"httpRequests" json:"httpRequests"`
	JourneyLabel   string `yaml:"journeyLabel" json:"journeyLabel"`
	ThresholdLabel string `yaml:"thresholdLabel" json:"thresholdLabel"`
	RunLabel       string `yaml:"runLabel" json:"runLabel"`
	StatusLabel    string `yaml:"statusLabel" json:"statusLabel"`
}

type contextQueries struct {
	GeneratorCPU string `yaml:"generatorCPU" json:"generatorCPU"`
	NodeCPU      string `yaml:"nodeCPU" json:"nodeCPU"`
	Nodes        string `yaml:"nodes" json:"nodes"`
	FrontendRPS  string `yaml:"frontendRPS" json:"frontendRPS"`
}

type costModel struct {
	Currency              string  `yaml:"currency" json:"currency"`
	Nodes                 int     `yaml:"nodes" json:"nodes"`
	MachineType           string  `yaml:"machineType" json:"machineType"`
	Provisioning          string  `yaml:"provisioning" json:"provisioning"`
	Region                string  `yaml:"region" json:"region"`
	NodeHourly            float64 `yaml:"nodeHourly" json:"nodeHourly"`
	DiskGBPerNode         float64 `yaml:"diskGBPerNode" json:"diskGBPerNode"`
	DiskGBMonthly         float64 `yaml:"diskGBMonthly" json:"diskGBMonthly"`
	HoursPerMonth         float64 `yaml:"hoursPerMonth" json:"hoursPerMonth"`
	AllocatableCPUPerNode float64 `yaml:"allocatableCPUPerNode" json:"allocatableCPUPerNode"`
	Source                string  `yaml:"source" json:"source"`
}

// duration reads "60s" or "5m" from YAML and writes the same form to JSON.
type duration struct{ time.Duration }

func (d *duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	d.Duration = v
	return nil
}

func (d duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

var (
	promMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	promLabelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	gkeName        = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$`)
)

// loadConfig starts from the built-in config.yaml and lays the file at path (if any) over it.
func loadConfig(path string) (config, error) {
	var cfg config
	if err := decodeYAMLStrict(defaultConfigYAML, &cfg); err != nil {
		return config{}, fmt.Errorf("built-in config: %w", err)
	}
	if path != "" {
		data, err := os.ReadFile(path) // #nosec G304 -- the operator names the file
		if err != nil {
			return config{}, err
		}
		if err := decodeYAMLStrict(data, &cfg); err != nil {
			return config{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := cfg.validate(); err != nil {
		return config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// decodeYAMLStrict rejects unknown keys, so a typo cannot silently keep a default.
func decodeYAMLStrict(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// validate reports every problem at once.
func (c config) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	positive := func(v float64) bool { return v > 0 && !math.IsInf(v, 0) }

	check(gkeName.MatchString(c.Cluster), "cluster %q is not a GKE cluster name", c.Cluster)
	check(c.Warmup.Duration >= 0 && c.Warmup.Duration%time.Second == 0, "warmup %s must be whole seconds, 0 or more", c.Warmup.Duration)
	check(c.MinDeliveredRatio > 0 && c.MinDeliveredRatio <= 1, "minDeliveredRatio %g must be above 0 and at most 1", c.MinDeliveredRatio)
	check(c.MinRequestsPerJourney >= 1, "minRequestsPerJourney %g must be at least 1", c.MinRequestsPerJourney)
	check(positive(c.GeneratorCPULimit), "generatorCPULimit %g must be above 0", c.GeneratorCPULimit)
	check(c.RateLimitShare > 0 && c.RateLimitShare <= 1, "rateLimitShare %g must be above 0 and at most 1", c.RateLimitShare)

	type named struct{ key, name string }
	for _, n := range []named{
		{"metrics.requests", c.Metrics.Requests}, {"metrics.good", c.Metrics.Good}, {"metrics.fast", c.Metrics.Fast},
		{"metrics.dropped", c.Metrics.Dropped}, {"metrics.duration", c.Metrics.Duration},
		{"metrics.httpRequests", c.Metrics.HTTPRequests},
	} {
		check(promMetricName.MatchString(n.name), "%s %q is not a Prometheus metric name", n.key, n.name)
	}
	for _, n := range []named{
		{"metrics.journeyLabel", c.Metrics.JourneyLabel}, {"metrics.thresholdLabel", c.Metrics.ThresholdLabel},
		{"metrics.runLabel", c.Metrics.RunLabel}, {"metrics.statusLabel", c.Metrics.StatusLabel},
	} {
		check(promLabelName.MatchString(n.name), "%s %q is not a Prometheus label name", n.key, n.name)
	}

	m := c.Cost
	check(m.Currency != "", "cost.currency is empty")
	check(m.Nodes >= 1, "cost.nodes %d must be at least 1", m.Nodes)
	check(positive(m.NodeHourly), "cost.nodeHourly %g must be above 0", m.NodeHourly)
	check(m.DiskGBPerNode >= 0 && m.DiskGBMonthly >= 0, "cost.diskGBPerNode and cost.diskGBMonthly must be 0 or more")
	check(positive(m.HoursPerMonth), "cost.hoursPerMonth %g must be above 0", m.HoursPerMonth)
	check(positive(m.AllocatableCPUPerNode), "cost.allocatableCPUPerNode %g must be above 0", m.AllocatableCPUPerNode)
	return errors.Join(errs...)
}
