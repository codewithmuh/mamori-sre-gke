package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Fixed by the contract and the RBAC in deploy/executor: not configurable on purpose.
const (
	targetNamespace = "boutique"
	configMapName   = "executor-config"
	stateMapName    = "executor-state"
	faultLabel      = "mamori.dev/fault"
	faultIDLabel    = "mamori.dev/fault-id"

	// hardMaxReplicas caps every configured bound. The admission policy in deploy/executor
	// enforces the same number, so a config typo cannot scale a service to 50.
	hardMaxReplicas = 5
	defaultTTL      = 15 * time.Minute
	minTTL          = time.Minute
	maxTTL          = time.Hour
)

var (
	slackChannelPattern = regexp.MustCompile(`^[CG][A-Z0-9]{2,20}$`)
	slackUserPattern    = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
	dns1123Label        = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dns1123Subdomain    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// Bounds are the replica counts scale may set for one Deployment, inclusive.
type Bounds struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}

// Config is ConfigMap executor-config: the kill switch in key "enabled" and the rest in
// key "config.yaml". An action type is allowed only if its section is present.
type Config struct {
	Enabled bool `json:"-"`

	Slack struct {
		Channel   string   `json:"channel"`
		Approvers []string `json:"approvers"`
	} `json:"slack"`
	ProposalTTL string `json:"proposalTTL,omitempty"`

	Restart *struct {
		Deployments []string `json:"deployments"`
	} `json:"restart,omitempty"`
	Scale       map[string]Bounds `json:"scale,omitempty"`
	RemoveFault *struct {
		Kinds []string `json:"kinds"`
	} `json:"removeFault,omitempty"`
	// Rollback is here only to give a clear error: it cannot be enabled (see policy.go).
	Rollback json.RawMessage `json:"rollback,omitempty"`

	ttl time.Duration
}

// parseConfig reads the ConfigMap data and reports every problem at once.
func parseConfig(data map[string]string) (*Config, error) {
	cfg := &Config{}
	raw := strings.TrimSpace(data["config.yaml"])
	if raw == "" {
		return nil, errors.New(`key "config.yaml" is missing or empty`)
	}
	if err := yaml.UnmarshalStrict([]byte(raw), cfg); err != nil {
		return nil, fmt.Errorf("config.yaml: %w", err)
	}
	// Anything but the exact string "true" keeps the executor off: fail closed.
	cfg.Enabled = strings.TrimSpace(data["enabled"]) == "true"

	var errs []error
	if c := cfg.Slack.Channel; c != "" && !slackChannelPattern.MatchString(c) {
		errs = append(errs, fmt.Errorf("slack.channel %q is not a Slack channel id (C... or G...)", c))
	}
	for _, u := range cfg.Slack.Approvers {
		if !slackUserPattern.MatchString(u) {
			errs = append(errs, fmt.Errorf("slack.approvers: %q is not a Slack user id (U... or W...)", u))
		}
	}
	cfg.ttl = defaultTTL
	if cfg.ProposalTTL != "" {
		d, err := time.ParseDuration(cfg.ProposalTTL)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("proposalTTL %q: %w", cfg.ProposalTTL, err))
		case d < minTTL || d > maxTTL:
			errs = append(errs, fmt.Errorf("proposalTTL %s must be between %s and %s", d, minTTL, maxTTL))
		default:
			cfg.ttl = d
		}
	}
	if cfg.Restart != nil {
		for _, d := range cfg.Restart.Deployments {
			if !dns1123Label.MatchString(d) {
				errs = append(errs, fmt.Errorf("restart.deployments: %q is not a Deployment name", d))
			}
		}
	}
	for name, b := range cfg.Scale {
		if !dns1123Label.MatchString(name) {
			errs = append(errs, fmt.Errorf("scale: %q is not a Deployment name", name))
		}
		if b.Min < 1 || b.Min > b.Max || b.Max > hardMaxReplicas {
			errs = append(errs, fmt.Errorf("scale.%s: need 1 <= min (%d) <= max (%d) <= %d", name, b.Min, b.Max, hardMaxReplicas))
		}
	}
	if cfg.RemoveFault != nil {
		for _, k := range cfg.RemoveFault.Kinds {
			if _, ok := faultKinds[k]; !ok {
				errs = append(errs, fmt.Errorf("removeFault.kinds: %q is not supported (supported: %s)", k, strings.Join(sortedFaultKinds(), ", ")))
			}
		}
	}
	if len(cfg.Rollback) > 0 {
		errs = append(errs, errors.New("rollback cannot be enabled: Argo CD self-heal undoes a cluster-side rollback; roll back by reverting the commit in Git"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// allows reports whether an action type is in the allow-list.
func (c *Config) allows(actionType string) bool {
	switch actionType {
	case actionRestart:
		return c.Restart != nil && len(c.Restart.Deployments) > 0
	case actionScale:
		return len(c.Scale) > 0
	case actionRemoveFault:
		return c.RemoveFault != nil && len(c.RemoveFault.Kinds) > 0
	default:
		return false
	}
}

// approvalPath is nil when a proposal could be approved by someone.
func (c *Config) approvalPath() error {
	if c.Slack.Channel == "" || len(c.Slack.Approvers) == 0 {
		return errors.New("no Slack channel or no approvers in executor-config, so nobody could approve")
	}
	return nil
}

func (c *Config) isApprover(user string) bool { return slices.Contains(c.Slack.Approvers, user) }

// configSource returns the current configuration. Callers must treat an error as "off".
type configSource interface {
	Get(ctx context.Context) (*Config, error)
}

// configMapSource reads executor-config on every call, so a kill switch flipped with kubectl
// counts from the next check. It re-parses only when the resourceVersion changes.
type configMapSource struct {
	client    kubernetes.Interface
	namespace string

	mu    sync.Mutex
	rv    string
	cfg   *Config
	cfgEr error
}

// Get returns the parsed configuration, or an error that callers treat as "off".
func (s *configMapSource) Get(ctx context.Context) (*Config, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("ConfigMap %s/%s not found", s.namespace, configMapName)
	}
	if err != nil {
		return nil, fmt.Errorf("read ConfigMap %s/%s: %w", s.namespace, configMapName, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cm.ResourceVersion == "" || cm.ResourceVersion != s.rv {
		s.cfg, s.cfgEr = parseConfig(cm.Data)
		s.rv = cm.ResourceVersion
	}
	if s.cfgEr != nil {
		return nil, fmt.Errorf("ConfigMap %s/%s is invalid: %w", s.namespace, configMapName, s.cfgEr)
	}
	cp := *s.cfg
	return &cp, nil
}
