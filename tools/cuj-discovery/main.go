// Command cuj-discovery finds which upstream services each critical user
// journey (CUJ) of the shop depends on. It breaks one service at a time,
// runs every journey through the prober's on-demand API, and writes the
// result to slo/cuj-map.yaml, which the SLO generator reads.
//
// It changes only namespace boutique, labels everything it creates with
// mamori.dev/fault=true, and removes each fault before it moves on or exits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"go.yaml.in/yaml/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

// deps are the outside world, replaced in tests.
type deps struct {
	connectKube   func(kubeconfig, kubeContext string) (*kubeClients, error)
	connectProber func(ctx context.Context, kc *kubeClients, proberURL string, log *slog.Logger) (prober, func(), error)
	now           func() time.Time
	sleep         func(context.Context, time.Duration) error
	poll          time.Duration
}

var defaultDeps = deps{
	connectKube:   newKubeClients,
	connectProber: connectProber,
	now:           time.Now,
	sleep:         sleepCtx,
	poll:          2 * time.Second,
}

func main() {
	// The first Ctrl-C cancels the run, and the faults are removed before the
	// program exits. Further Ctrl-Cs are ignored until then, so the clean-up
	// can't be cut short by accident.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDeps)
	stop()
	os.Exit(code)
}

const usage = `cuj-discovery finds which services each user journey depends on. It breaks one
upstream service at a time (Istio abort; redis-cart: scale to zero), runs every
journey through the prober, and writes the result as a CUJ map. It only ever
changes namespace boutique.

Usage:
  cuj-discovery run [flags]        measure, then write slo/cuj-map.yaml
  cuj-discovery cleanup [flags]    remove every fault this tool left behind
  cuj-discovery validate [file]    check a CUJ map (default slo/cuj-map.yaml)
  cuj-discovery version

Run "cuj-discovery <command> -h" for its flags. Docs: docs/slo/cuj-discovery.md
`

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 0 {
		say(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr, d)
	case "cleanup":
		return cmdCleanup(ctx, args[1:], stdout, stderr, d)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		say(stdout, "cuj-discovery %s\n", version)
		return 0
	case "help", "-h", "-help", "--help":
		say(stdout, "%s", usage)
		return 0
	default:
		say(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// runConfig is everything `run` takes from its flags.
type runConfig struct {
	kubeconfig     string
	kubeContext    string
	proberURL      string
	out            string
	onlineBoutique string
	timeout        time.Duration
	dryRun         bool
	opts           options
}

func parseRunFlags(args []string, stderr io.Writer) (runConfig, error) {
	fs := flag.NewFlagSet("cuj-discovery run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c runConfig
	var cujs, services string
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	fs.StringVar(&c.kubeContext, "context", "", "kubeconfig context (default: the current context)")
	fs.StringVar(&c.proberURL, "prober-url", "", "prober base URL, e.g. http://127.0.0.1:8080 (default: a port-forward to service prober/prober:8080)")
	fs.StringVar(&c.out, "out", "slo/cuj-map.yaml", "where to write the CUJ map")
	fs.StringVar(&c.onlineBoutique, "online-boutique", "v0.10.7", "Online Boutique version to record in the map")
	fs.StringVar(&cujs, "cujs", "", "comma-separated journeys to measure (default: all four)")
	fs.StringVar(&services, "services", "", "comma-separated services to break (default: all ten)")
	fs.IntVar(&c.opts.Runs, "runs", 20, "runs per journey for each broken service (1-100)")
	fs.IntVar(&c.opts.BaselineRuns, "baseline-runs", 10, "runs per journey before any fault; all must pass (1-100)")
	fs.IntVar(&c.opts.RecoveryRuns, "recovery-runs", 2, "runs per journey per check while waiting for recovery (1-100)")
	fs.Float64Var(&c.opts.Threshold, "threshold", 0.5, "failure rate at or above which a service is critical (above 0, at most 1)")
	fs.DurationVar(&c.opts.Settle, "settle", 15*time.Second, "wait after a fault is in place before measuring")
	fs.DurationVar(&c.opts.RecoveryTimeout, "recovery-timeout", 6*time.Minute, "how long the shop may take to recover after each fault")
	fs.DurationVar(&c.timeout, "timeout", 90*time.Minute, "stop the whole run (and remove the fault) after this long")
	fs.BoolVar(&c.dryRun, "dry-run", false, "check the cluster and print the plan and objects; change nothing, call no prober")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	c.opts.RecoveryInterval = 10 * time.Second
	c.opts.RemoveTimeout = 2 * time.Minute

	var errs []error
	var err error
	if c.opts.CUJs, err = selectCUJs(cujs); err != nil {
		errs = append(errs, err)
	}
	if c.opts.Targets, err = selectTargets(services); err != nil {
		errs = append(errs, err)
	}
	for _, n := range []struct {
		name string
		v    int
	}{{"-runs", c.opts.Runs}, {"-baseline-runs", c.opts.BaselineRuns}, {"-recovery-runs", c.opts.RecoveryRuns}} {
		if n.v < 1 || n.v > maxProberRuns {
			errs = append(errs, fmt.Errorf("%s=%d must be 1-%d (the prober's limit)", n.name, n.v, maxProberRuns))
		}
	}
	if t := c.opts.Threshold; math.IsNaN(t) || t <= 0 || t > 1 {
		errs = append(errs, fmt.Errorf("-threshold=%g must be above 0 and at most 1", c.opts.Threshold))
	}
	if c.opts.Settle < 0 || c.opts.Settle > 10*time.Minute {
		errs = append(errs, fmt.Errorf("-settle=%s must be 0-10m", c.opts.Settle))
	}
	if c.opts.RecoveryTimeout <= 0 || c.timeout <= 0 {
		errs = append(errs, errors.New("-recovery-timeout and -timeout must be positive"))
	}
	if strings.TrimSpace(c.out) == "" {
		errs = append(errs, errors.New("-out must not be empty"))
	}
	if c.proberURL != "" {
		u, err := url.Parse(c.proberURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("-prober-url=%q must be an http(s) URL such as http://127.0.0.1:8080", c.proberURL))
		}
	}
	return c, errors.Join(errs...)
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	c, err := parseRunFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		say(stderr, "error: %v\n", err)
		return 2
	}
	log := newLogger(stderr)
	runID := strings.ToLower(d.now().UTC().Format("20060102T150405Z"))

	kc, err := d.connectKube(c.kubeconfig, c.kubeContext)
	if err != nil {
		return fail(stderr, log, err)
	}
	log.InfoContext(ctx, "starting", "run", runID, "context", kc.context, "server", kc.server, "namespace", shopNamespace,
		"dry_run", c.dryRun, "cujs", c.opts.CUJs, "services", targetNames(c.opts.Targets))
	cl := &cluster{kube: kc.kube, dyn: kc.dyn, runID: runID, log: log, sleep: d.sleep, poll: d.poll, podsGoneTimeout: 3 * time.Minute}

	if c.dryRun {
		perr := cl.preflight(ctx, c.opts.Targets)
		replicas := map[string]int32{}
		for _, t := range c.opts.Targets {
			if t.Method == methodScaleToZero {
				replicas[t.Service] = 1
				if dep, err := kc.kube.AppsV1().Deployments(shopNamespace).Get(ctx, t.Service, metav1.GetOptions{}); err == nil {
					replicas[t.Service] = replicasOf(dep.Spec.Replicas)
				}
			}
		}
		if err := printPlan(stdout, c, runID, kc, replicas, perr); err != nil {
			return fail(stderr, log, err)
		}
		if perr != nil {
			return fail(stderr, log, fmt.Errorf("preflight: a real run would refuse to start: %w", perr))
		}
		return 0
	}

	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	p, stop, err := d.connectProber(runCtx, kc, c.proberURL, log)
	if err != nil {
		return fail(stderr, log, err)
	}
	defer stop()

	disc := &discoverer{opts: c.opts, faults: cl, prober: p, log: log, sleep: d.sleep}
	res, err := disc.discover(runCtx)

	// Whatever happened, nothing from this run may stay behind. Each fault was
	// already removed on the way out; this catches the rest (say, a create
	// that reached the API server although the client saw an error).
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	left, serr := cl.sweep(sctx, ourSelector+","+labelRun+"="+runID, false)
	scancel()
	if len(left) > 0 {
		log.WarnContext(ctx, "removed faults that were still in place", "objects", left)
	}
	if serr != nil {
		err = errors.Join(err, fmt.Errorf("final clean-up: %w (run: cuj-discovery cleanup)", serr))
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
			err = fmt.Errorf("interrupted; faults removed: %w", err)
		case runCtx.Err() != nil:
			err = fmt.Errorf("stopped by -timeout %s; faults removed: %w", c.timeout, err)
		}
		return fail(stderr, log, err)
	}

	m := buildMap(res, c.opts, mapMetadata{
		GeneratedAt:      d.now().UTC().Format(time.RFC3339),
		Generator:        "cuj-discovery/" + version,
		OnlineBoutique:   c.onlineBoutique,
		RunsPerCase:      c.opts.Runs,
		FailureThreshold: c.opts.Threshold,
		Fault:            faultDescription(c.opts.Settle),
	})
	if err := validateMap(m); err != nil {
		return fail(stderr, log, fmt.Errorf("the generated map is invalid (a bug; nothing written): %w", err))
	}
	data, err := renderMap(m)
	if err == nil {
		err = writeFileAtomic(c.out, data)
	}
	if err != nil {
		return fail(stderr, log, fmt.Errorf("write %s: %w", c.out, err))
	}
	log.InfoContext(ctx, "CUJ map written", "path", c.out, "run", runID)
	printSummary(stdout, m, c.out)
	return 0
}

func cmdCleanup(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	fs := flag.NewFlagSet("cuj-discovery cleanup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	kubeContext := fs.String("context", "", "kubeconfig context (default: the current context)")
	dryRun := fs.Bool("dry-run", false, "list what would be removed; change nothing")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up after this long")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		say(stderr, "error: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	log := newLogger(stderr)
	kc, err := d.connectKube(*kubeconfig, *kubeContext)
	if err != nil {
		return fail(stderr, log, err)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	log.InfoContext(ctx, "cleanup", "context", kc.context, "server", kc.server, "namespace", shopNamespace, "dry_run", *dryRun)
	cl := &cluster{kube: kc.kube, dyn: kc.dyn, log: log, sleep: d.sleep, poll: d.poll}

	removed, err := cl.sweep(ctx, ourSelector, *dryRun)
	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	if len(removed) == 0 && err == nil {
		say(stdout, "nothing to remove: no cuj-discovery faults in namespace %s\n", shopNamespace)
	}
	for _, r := range removed {
		say(stdout, "%s %s\n", verb, r)
	}
	if err != nil {
		return fail(stderr, log, err)
	}
	if !*dryRun {
		// Anything still labelled as a fault was made by someone else (a game
		// day): report it, never touch it.
		if others, err := cl.existingFaults(ctx); err == nil && len(others) > 0 {
			say(stdout, "left alone (not created by cuj-discovery): %s\n", strings.Join(others, ", "))
		}
	}
	return 0
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cuj-discovery validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := "slo/cuj-map.yaml"
	switch fs.NArg() {
	case 0:
	case 1:
		path = fs.Arg(0)
	default:
		say(stderr, "error: validate takes one file\n")
		return 2
	}
	data, err := os.ReadFile(path) //nolint:gosec // reading the file the user named is the point
	if err != nil {
		say(stderr, "error: %v\n", err)
		return 1
	}
	m, err := parseCUJMap(data)
	if err != nil {
		say(stderr, "%s is not a valid CUJ map:\n%v\n", path, err)
		return 1
	}
	say(stdout, "%s: valid (%d journeys, %d runs per case, threshold %g)\n",
		path, len(m.Journeys), m.Metadata.RunsPerCase, m.Metadata.FailureThreshold)
	return 0
}

// say writes text for the person at the terminal. If stdout or stderr can't
// be written there is nowhere better to report it, so the error is dropped.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func fail(stderr io.Writer, log *slog.Logger, err error) int {
	log.Error("failed", "error", err.Error())
	say(stderr, "error: %v\n", err)
	return 1
}

func targetNames(ts []target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Service
	}
	return out
}

// printPlan shows what a real run would do, including every object it would
// create, so a reviewer can check them before anything changes.
func printPlan(w io.Writer, c runConfig, runID string, kc *kubeClients, replicas map[string]int32, preflightErr error) error {
	var b strings.Builder
	p := func(format string, a ...any) { say(&b, format+"\n", a...) }
	p("Dry run %s: nothing is changed, the prober is not called, no map is written.", runID)
	p("Cluster: context %q, server %s. Namespace %s is the only one the tool changes.", kc.context, kc.server, shopNamespace)
	if preflightErr != nil {
		p("Preflight: FAILED, a real run would refuse to start:\n  %s", strings.ReplaceAll(preflightErr.Error(), "\n", "\n  "))
	} else {
		p("Preflight: passed (no fault objects in %s; every target can be broken as planned).", shopNamespace)
	}
	proberDesc := c.proberURL
	if proberDesc == "" {
		proberDesc = fmt.Sprintf("port-forward to service %s/%s:%d", proberNamespace, proberService, proberPort)
	}
	p("Prober: %s", proberDesc)
	p("")
	p("Plan:")
	p("1. Baseline: run each journey (%s) %d times. Stop unless every run passes.", strings.Join(c.opts.CUJs, ", "), c.opts.BaselineRuns)
	p("2. One service at a time:")
	for _, t := range c.opts.Targets {
		switch t.Method {
		case methodGRPCAbort:
			p("   - %s: create VirtualService %s/%s (gRPC UNAVAILABLE for 100 %% of requests)", t.Service, shopNamespace, objectName(t.Service))
		case methodScaleToZero:
			p("   - %s: create marker ConfigMap %s/%s, scale Deployment %s/%s from %d to 0 replicas, wait until its pods are gone",
				t.Service, shopNamespace, objectName(t.Service), shopNamespace, t.Service, replicas[t.Service])
		}
	}
	p("   For each: wait %s, run each journey %d times, check the fault is still in place, remove it", c.opts.Settle, c.opts.Runs)
	p("   (scale back and delete the marker), then wait up to %s until every journey passes again.", c.opts.RecoveryTimeout)
	p("3. Delete anything still labelled %s,%s=%s, then write %s", ourSelector, labelRun, runID, c.out)
	p("   (critical when the failure rate is at least %g). Whole run limited to %s.", c.opts.Threshold, c.timeout)
	p("")
	p("Objects it would create:")
	for _, t := range c.opts.Targets {
		obj := abortVirtualService(t.Service, runID)
		if t.Method == methodScaleToZero {
			obj = markerConfigMap(t.Service, runID, replicas[t.Service])
		}
		data, err := yaml.Marshal(obj.Object)
		if err != nil {
			return err
		}
		p("---")
		_, _ = b.Write(data)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func printSummary(w io.Writer, m cujMap, path string) {
	say(w, "Wrote %s: %d runs per case, critical when the failure rate is at least %g.\n\n",
		path, m.Metadata.RunsPerCase, m.Metadata.FailureThreshold)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	say(tw, "JOURNEY\tCRITICAL DEPENDENCIES\n")
	var partial []string
	for _, j := range m.Journeys {
		say(tw, "%s\t%s\n", j.ID, strings.Join(j.Critical, ", "))
		for _, s := range append(append([]string{}, j.Critical...), j.NonCritical...) {
			if r := j.Results[s].FailureRate; r > 0 && r < 1 {
				partial = append(partial, fmt.Sprintf("%s/%s=%s", j.ID, s, formatRate(r)))
			}
		}
	}
	_ = tw.Flush()
	if len(partial) > 0 {
		say(w, "\nPartial failure rates (neither 0 nor 1), worth a look or a re-run: %s\n", strings.Join(partial, ", "))
	}
}
