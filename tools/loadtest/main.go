// Command loadtest turns a k6 step-load run (deploy/loadtest) into a capacity report. For each step
// it reads the journeys' SLIs from Managed Service for Prometheus, finds the highest step at which
// every SLO still held, and prices that rate per 1,000 requests per second.
//
//	loadtest report --project PROJECT_ID --summary loadtest-results/RUN/summary.json
//	loadtest report --project PROJECT_ID --from 2027-01-12T10:00:00Z --steps 5,10,20 --step-duration 5m
//
// The method is in docs/capacity/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

const usageText = `Usage: loadtest report [flags]

Reads a k6 step-load run's SLIs per step from Managed Service for Prometheus (or any
Prometheus API), finds the highest step at which every SLO in slo/slos.yaml held, and
prices that rate per 1,000 requests per second. Logs go to stderr, the report to stdout
or --out.

Flags:
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// errUsage marks bad command lines: exit code 2 instead of 1.
var errUsage = errors.New("usage")

// say writes a message for the operator. Nothing useful can be done if the terminal is gone.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	log := newLogger(stderr)
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		out, code := stdout, 0
		if len(args) == 0 {
			out, code = stderr, 2
		}
		reportFlags(&reportOptions{}, out).Usage()
		return code
	}
	if args[0] != "report" {
		say(stderr, "unknown command %q; the only command is report (see loadtest help)\n", args[0])
		return 2
	}
	if err := runReport(ctx, args[1:], stdout, stderr, log); err != nil {
		if errors.Is(err, errUsage) {
			return 2
		}
		log.Error("report failed", "error", err.Error())
		return 1
	}
	return 0
}

type reportOptions struct {
	project, prometheusURL, summary, from, to, steps, runID string
	slos, configPath, format, out, jsonOut                  string
	stepDuration, timeout                                   time.Duration
}

func reportFlags(o *reportOptions, output io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		say(fs.Output(), "%s", usageText)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.project, "project", "", "Google Cloud project: query its Managed Service for Prometheus with Application Default Credentials")
	fs.StringVar(&o.prometheusURL, "prometheus-url", "", "query this Prometheus-compatible API instead, without authentication (e.g. http://localhost:9090)")
	fs.StringVar(&o.summary, "summary", "", "k6 summary JSON of the run (saved by make loadtest-run): gives the start, steps, step duration and run id")
	fs.StringVar(&o.from, "from", "", "start of step 1, RFC 3339 (default: from --summary)")
	fs.StringVar(&o.to, "to", "", "end of the time range, RFC 3339; steps not finished by then are not evaluated (default: from --summary, else now)")
	fs.StringVar(&o.steps, "steps", "", "requests per second of each step, comma-separated, as in the run (default: from --summary)")
	fs.DurationVar(&o.stepDuration, "step-duration", 0, "length of each step, as in the run (default: from --summary)")
	fs.StringVar(&o.runID, "run-id", "", "count only k6 metrics with this run label (default: from --summary)")
	fs.StringVar(&o.slos, "slos", "slo/slos.yaml", "SLO spec with each journey's targets")
	fs.StringVar(&o.configPath, "config", "", "YAML file overriding the built-in settings (tools/loadtest/config.yaml)")
	fs.StringVar(&o.format, "format", "markdown", "report format: markdown or json")
	fs.StringVar(&o.out, "out", "", "write the report to this file instead of stdout")
	fs.StringVar(&o.jsonOut, "json-out", "", "also write the report as JSON to this file")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "time limit for all queries")
	return fs
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func runReport(ctx context.Context, args []string, stdout, stderr io.Writer, log *slog.Logger) error {
	var o reportOptions
	fs := reportFlags(&o, stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		say(stderr, "unexpected arguments: %v\n", fs.Args())
		return errUsage
	}
	if (o.project == "") == (o.prometheusURL == "") {
		say(stderr, "give exactly one of --project or --prometheus-url\n")
		return errUsage
	}
	if o.format != "markdown" && o.format != "json" {
		say(stderr, "--format %q: want markdown or json\n", o.format)
		return errUsage
	}

	cfg, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	slos, err := loadSLOs(o.slos)
	if err != nil {
		return err
	}
	plan, err := resolvePlan(o, time.Now())
	if err != nil {
		return err
	}
	if err := checkThresholds(slos, plan.LatencyThresholdsMs); err != nil {
		return err
	}
	steps, err := buildSchedule(plan.Start, plan.Steps, plan.StepDuration, cfg.Warmup.Duration)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	var (
		q      *promAPI
		source string
	)
	if o.project != "" {
		q, err = newManagedPrometheus(ctx, o.project)
		source = "Managed Service for Prometheus in project `" + o.project + "`"
	} else {
		q, err = newPrometheus(o.prometheusURL)
		source = "the Prometheus API at " + o.prometheusURL
	}
	if err != nil {
		return err
	}

	log.Info("evaluating run", "run_id", plan.RunID, "from", plan.Start, "to", plan.End, "steps", len(steps))
	ev := &evaluator{q: q, cfg: cfg, slos: slos, runID: plan.RunID, log: log}
	results, err := ev.evaluate(ctx, steps, plan.End)
	if err != nil {
		return err
	}
	r := buildReport(cfg, slos, plan, results, source, time.Now())
	return writeReport(r, o, stdout)
}

// resolvePlan takes the schedule from the summary, then lets each explicit flag override it.
func resolvePlan(o reportOptions, now time.Time) (runPlan, error) {
	var plan runPlan
	if o.summary != "" {
		p, err := readK6Summary(o.summary)
		if err != nil {
			return runPlan{}, err
		}
		plan = p
	}
	if o.from != "" {
		t, err := time.Parse(time.RFC3339, o.from)
		if err != nil {
			return runPlan{}, fmt.Errorf("--from: %w", err)
		}
		plan.Start = t
	}
	if o.to != "" {
		t, err := time.Parse(time.RFC3339, o.to)
		if err != nil {
			return runPlan{}, fmt.Errorf("--to: %w", err)
		}
		plan.End = t
	}
	if o.steps != "" {
		s, err := parseSteps(o.steps)
		if err != nil {
			return runPlan{}, err
		}
		plan.Steps = s
	}
	if o.stepDuration != 0 {
		plan.StepDuration = o.stepDuration
	}
	if o.runID != "" {
		plan.RunID = o.runID
	}

	var errs []error
	if plan.Start.IsZero() {
		errs = append(errs, errors.New("no start time: give --from or --summary"))
	}
	if len(plan.Steps) == 0 {
		errs = append(errs, errors.New("no steps: give --steps or --summary"))
	}
	if plan.StepDuration <= 0 {
		errs = append(errs, errors.New("no step duration: give --step-duration or --summary"))
	}
	if plan.RunID != "" && !runIDPattern.MatchString(plan.RunID) {
		errs = append(errs, fmt.Errorf("run id %q may only hold letters, digits, '.', '_' and '-'", plan.RunID))
	}
	if plan.End.IsZero() {
		plan.End = now
	}
	if !plan.Start.IsZero() && !plan.End.After(plan.Start) {
		errs = append(errs, fmt.Errorf("the time range ends (%s) before it starts (%s)", plan.End.Format(time.RFC3339), plan.Start.Format(time.RFC3339)))
	}
	return plan, errors.Join(errs...)
}

// checkThresholds fails early when the run's summary shows it did not count requests under an SLO's
// latency threshold. Without a summary the evaluator finds out from the data instead.
func checkThresholds(slos sloSpec, measured []float64) error {
	if len(measured) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, t := range measured {
		have[thresholdLabel(t)] = true
	}
	var errs []error
	for _, j := range slos.Journeys {
		if l := thresholdLabel(j.Latency.ThresholdMs); !have[l] {
			errs = append(errs, fmt.Errorf("%s: the SLO's threshold is %s ms, but the run only counted requests under %v ms; add it to LATENCY_THRESHOLDS_MS and run again",
				j.ID, l, measured))
		}
	}
	return errors.Join(errs...)
}

func buildReport(cfg config, slos sloSpec, plan runPlan, results []stepResult, source string, now time.Time) report {
	c := findCapacity(results)
	r := report{
		GeneratedAt:         now.UTC(),
		Source:              source,
		RunID:               plan.RunID,
		From:                plan.Start.UTC(),
		To:                  plan.End.UTC(),
		StepDuration:        duration{plan.StepDuration},
		Warmup:              cfg.Warmup,
		Cluster:             cfg.Cluster,
		MinDeliveredRatio:   cfg.MinDeliveredRatio,
		MinRequests:         cfg.MinRequestsPerJourney,
		ConfidenceZ:         cfg.ConfidenceZ,
		CostModel:           cfg.Cost,
		SLOs:                slos.Journeys,
		Steps:               results,
		MaxStep:             c.Max,
		FirstNotPassingStep: c.FirstNotPass,
		RateLimitShare:      cfg.RateLimitShare,
		Warnings:            c.Warnings,
	}
	if c.Max != nil {
		best := results[*c.Max-1]
		if best.MeasuredRPS != nil && *best.MeasuredRPS > 0 {
			rps := *best.MeasuredRPS
			cost := priceCapacity(cfg.Cost, rps, best.GeneratorCPU)
			// Envoy's token bucket takes whole tokens per second, so round down.
			limit := math.Floor(rps * cfg.RateLimitShare)
			r.MaxRPS, r.Cost, r.SuggestedRateLimit = &rps, &cost, &limit
		}
	}
	return r
}

func writeReport(r report, o reportOptions, stdout io.Writer) error {
	write := writeMarkdown
	if o.format == "json" {
		write = writeJSON
	}
	var err error
	if o.out == "" {
		err = write(stdout, r)
	} else {
		err = writeFile(o.out, r, write)
	}
	if err == nil && o.jsonOut != "" {
		err = writeFile(o.jsonOut, r, writeJSON)
	}
	return err
}

func writeFile(path string, r report, write func(io.Writer, report) error) (err error) {
	f, err := os.Create(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return write(f, r)
}
