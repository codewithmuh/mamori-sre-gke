package spendguard

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Config comes only from environment variables, set by Terraform.
type Config struct {
	ProjectID   string  // PROJECT_ID
	Location    string  // LOCATION: the cluster's zone (or region)
	ClusterName string  // CLUSTER_NAME
	NodePool    string  // NODE_POOL
	Threshold   float64 // THRESHOLD: act when cost >= budget × THRESHOLD; default 1.0
	DryRun      bool    // DRY_RUN: log the planned actions only; default false
}

// Above this the guard would act only after overspending by more than the
// whole budget. A value like 100 is almost certainly a percentage typed by
// mistake, and would quietly disarm the guard.
const maxThreshold = 2.0

var (
	projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	locationPattern  = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+(-[a-z])?$`)
	gkeNamePattern   = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$`)
)

// loadConfig reads and checks every setting, and reports all problems at once.
func loadConfig(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }
	required := func(key string, pattern *regexp.Regexp) string {
		v := get(key)
		switch {
		case v == "":
			errs = append(errs, fmt.Errorf("%s is required", key))
		case !pattern.MatchString(v):
			errs = append(errs, fmt.Errorf("%s=%q is not valid (must match %s)", key, v, pattern))
		}
		return v
	}

	cfg := Config{
		ProjectID:   required("PROJECT_ID", projectIDPattern),
		Location:    required("LOCATION", locationPattern),
		ClusterName: required("CLUSTER_NAME", gkeNamePattern),
		NodePool:    required("NODE_POOL", gkeNamePattern),
		Threshold:   1.0,
	}

	if v := get("THRESHOLD"); v != "" {
		t, err := strconv.ParseFloat(v, 64)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("THRESHOLD=%q is not a number", v))
		case math.IsNaN(t) || t <= 0 || t > maxThreshold:
			errs = append(errs, fmt.Errorf("THRESHOLD=%q must be a fraction of the budget above 0 and at most %g (1.0 = 100 %%)", v, maxThreshold))
		default:
			cfg.Threshold = t
		}
	}

	if v := get("DRY_RUN"); v != "" {
		d, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("DRY_RUN=%q must be true or false", v))
		}
		cfg.DryRun = d
	}

	return cfg, errors.Join(errs...)
}

func (c Config) nodePoolName() string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s", c.ProjectID, c.Location, c.ClusterName, c.NodePool)
}

func (c Config) operationName(op string) string {
	return fmt.Sprintf("projects/%s/locations/%s/operations/%s", c.ProjectID, c.Location, op)
}

func (c Config) logAttrs() []any {
	return []any{
		"node_pool", c.nodePoolName(),
		"threshold", c.Threshold,
		"dry_run", c.DryRun,
	}
}
