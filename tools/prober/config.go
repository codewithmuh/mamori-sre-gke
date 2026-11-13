package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Defaults match the in-cluster deployment in deploy/prober.
const (
	defaultTargetURL = "http://istio-ingressgateway.istio-ingress.svc.cluster.local"
	defaultHost      = "mamori.codewithmuh.com"
	defaultInterval  = 60 * time.Second
	// Longer than the route timeouts #27 plans for the public route (6 s for GET, 10 s for POST),
	// so a slow page shows up as the gateway's 504, which mesh metrics can attribute, rather
	// than as a client-side timeout.
	defaultStepTimeout    = 12 * time.Second
	defaultListenAddr     = ":8080"
	defaultRunConcurrency = 4
	maxRunConcurrency     = 10
)

// config comes only from environment variables.
type config struct {
	targetURL      *url.URL      // PROBER_TARGET_URL: where requests go (no trailing slash)
	host           string        // PROBER_HOST: Host header, the hostname the Istio Gateway accepts
	interval       time.Duration // PROBER_INTERVAL: time between scheduled probes of one journey
	stepTimeout    time.Duration // PROBER_STEP_TIMEOUT: limit for one HTTP step, body included
	scheduled      []string      // PROBER_CUJS: journeys probed on the schedule
	listenAddr     string        // PROBER_LISTEN_ADDR: metrics, health and the on-demand API
	runConcurrency int           // PROBER_RUN_CONCURRENCY: parallel runs within one on-demand call
	logLevel       slog.Level    // PROBER_LOG_LEVEL
}

var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)

// loadConfig reads and checks every setting and reports all problems at once. The returned config
// always has a usable log level, so the caller can log the error.
func loadConfig(getenv func(string) string) (config, error) {
	var errs []error
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := config{
		host:           defaultHost,
		interval:       defaultInterval,
		stepTimeout:    defaultStepTimeout,
		scheduled:      journeyIDs(),
		listenAddr:     defaultListenAddr,
		runConcurrency: defaultRunConcurrency,
		logLevel:       slog.LevelInfo,
	}

	if v := get("PROBER_LOG_LEVEL"); v != "" {
		if err := cfg.logLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("PROBER_LOG_LEVEL=%q must be debug, info, warn or error", v))
		}
	}

	target := get("PROBER_TARGET_URL")
	if target == "" {
		target = defaultTargetURL
	}
	u, err := url.Parse(target)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("PROBER_TARGET_URL=%q is not a URL", target))
	case (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		errs = append(errs, fmt.Errorf("PROBER_TARGET_URL=%q must be an http or https URL with a host", target))
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		errs = append(errs, fmt.Errorf("PROBER_TARGET_URL=%q must not have user info, a query or a fragment", target))
	default:
		u.Path = strings.TrimSuffix(u.Path, "/")
		u.RawPath = ""
		cfg.targetURL = u
	}

	if v := get("PROBER_HOST"); v != "" {
		v = strings.ToLower(v)
		if hostPattern.MatchString(v) {
			cfg.host = v
		} else {
			errs = append(errs, fmt.Errorf("PROBER_HOST=%q is not a hostname", v))
		}
	}

	duration := func(key string, def, lo, hi time.Duration) time.Duration {
		v := get(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < lo || d > hi {
			errs = append(errs, fmt.Errorf("%s=%q must be a duration from %s to %s, like 60s", key, v, lo, hi))
			return def
		}
		return d
	}
	cfg.interval = duration("PROBER_INTERVAL", defaultInterval, time.Second, time.Hour)
	cfg.stepTimeout = duration("PROBER_STEP_TIMEOUT", defaultStepTimeout, 100*time.Millisecond, 5*time.Minute)
	if cfg.stepTimeout > cfg.interval {
		errs = append(errs, fmt.Errorf("PROBER_STEP_TIMEOUT (%s) must not be longer than PROBER_INTERVAL (%s)", cfg.stepTimeout, cfg.interval))
	}

	if v := get("PROBER_CUJS"); v != "" {
		ids, err := parseCUJs(v)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.scheduled = ids
		}
	}

	if v := get("PROBER_LISTEN_ADDR"); v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			errs = append(errs, fmt.Errorf("PROBER_LISTEN_ADDR=%q must be host:port or :port", v))
		} else {
			cfg.listenAddr = v
		}
	}

	if v := get("PROBER_RUN_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxRunConcurrency {
			errs = append(errs, fmt.Errorf("PROBER_RUN_CONCURRENCY=%q must be a number from 1 to %d", v, maxRunConcurrency))
		} else {
			cfg.runConcurrency = n
		}
	}

	return cfg, errors.Join(errs...)
}

// parseCUJs reads a comma-separated list of journey ids. "none" turns the schedule off, which is
// useful when the prober runs on a laptop for on-demand runs only.
func parseCUJs(v string) ([]string, error) {
	if strings.EqualFold(v, "none") {
		return []string{}, nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Split(v, ",") {
		id = strings.TrimSpace(id)
		if _, ok := journeyByID(id); !ok {
			return nil, fmt.Errorf("PROBER_CUJS: unknown journey %q (known: %s, or none)", id, strings.Join(journeyIDs(), ", "))
		}
		if seen[id] {
			return nil, fmt.Errorf("PROBER_CUJS: %q is listed twice", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}
