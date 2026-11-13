// Command evidence assembles the weekly compliance evidence bundle.
//
// The evidence workflow (.github/workflows/evidence.yml) collects raw inputs: CI results, Trivy
// and gitleaks reports, an SBOM, a Terraform drift summary, cluster state and SLO status. This
// command reads them from one directory and writes bundle.json (fixed schema, see
// docs/compliance/evidence-bundle.md) and summary.md. It makes no network calls.
//
//	evidence -inputs bundle/inputs -out bundle
//
// An input that is missing or invalid is recorded as "not collected" with its reason; it never
// makes the command fail. The command fails only on bad flags or when it can't write its output.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

const (
	bundleFile  = "bundle.json"
	summaryFile = "summary.md"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr, time.Now))
}

type options struct {
	inputs string
	out    string
	now    time.Time
	source source
}

// run is main without the process: it returns the exit code (0 written, 1 failed, 2 bad usage).
func run(args []string, getenv func(string) string, stderr io.Writer, clock func() time.Time) int {
	log := newLogger(stderr)
	opts, err := parseFlags(args, getenv, stderr, clock)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		log.Error("invalid arguments", "error", err.Error())
		return 2
	}

	dir, err := openInputDir(opts.inputs)
	if err != nil {
		log.Error("cannot read inputs", "error", err.Error())
		return 1
	}
	defer func() { _ = dir.Close() }()

	b := assemble(dir, opts.source, opts.now)
	for _, si := range b.Sections.list() {
		log.Info("section", "section", si.key, "status", si.status, "attention", si.attention, "headline", si.headline)
	}
	if err := write(opts.out, b); err != nil {
		log.Error("cannot write the bundle", "error", err.Error())
		return 1
	}
	log.Info("bundle written", "out", opts.out, "collected", b.Overview.Collected,
		"not_collected", b.Overview.NotCollected, "attention", strings.Join(b.Overview.Attention, ","))
	return 0
}

// parseFlags reads the flags. The source fields default to the variables GitHub Actions sets, so
// the workflow needs no flags beyond -inputs and -out.
func parseFlags(args []string, getenv func(string) string, output io.Writer, clock func() time.Time) (options, error) {
	var o options
	var now string
	fs := flag.NewFlagSet("evidence", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&o.inputs, "inputs", "", "directory holding the collected input files (required)")
	fs.StringVar(&o.out, "out", "", "directory to write bundle.json and summary.md to (required)")
	fs.StringVar(&now, "now", "", "generation time, RFC 3339 (default: the current time); for reproducible output")
	fs.StringVar(&o.source.Repository, "repository", getenv("GITHUB_REPOSITORY"), "owner/name of the repository")
	fs.StringVar(&o.source.Commit, "commit", getenv("GITHUB_SHA"), "commit the evidence was collected at")
	fs.StringVar(&o.source.Ref, "ref", getenv("GITHUB_REF"), "Git ref of the run")
	fs.StringVar(&o.source.Workflow, "workflow", getenv("GITHUB_WORKFLOW"), "name of the workflow")
	fs.StringVar(&o.source.Event, "event", getenv("GITHUB_EVENT_NAME"), "event that started the run")
	fs.StringVar(&o.source.RunID, "run-id", getenv("GITHUB_RUN_ID"), "workflow run id")
	fs.StringVar(&o.source.RunAttempt, "run-attempt", getenv("GITHUB_RUN_ATTEMPT"), "workflow run attempt")
	fs.StringVar(&o.source.RunURL, "run-url", defaultRunURL(getenv), "link to the workflow run")
	if err := fs.Parse(args); err != nil {
		return o, err
	}

	var errs []error
	if fs.NArg() > 0 {
		errs = append(errs, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if o.inputs == "" {
		errs = append(errs, errors.New("-inputs is required"))
	}
	if o.out == "" {
		errs = append(errs, errors.New("-out is required"))
	}
	o.now = clock()
	if now != "" {
		t, err := time.Parse(time.RFC3339, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("-now: %w", err))
		}
		o.now = t
	}
	return o, errors.Join(errs...)
}

func defaultRunURL(getenv func(string) string) string {
	server, repo, id := getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || id == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, id)
}

// write puts bundle.json and summary.md into dir, creating it if needed.
func write(dir string, b bundle) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(b); err != nil {
		return fmt.Errorf("encode %s: %w", bundleFile, err)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	for name, content := range map[string][]byte{bundleFile: buf.Bytes(), summaryFile: []byte(renderSummary(b))} {
		if err := root.WriteFile(name, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// newLogger writes one JSON object per line, like the other tools in this repository.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
