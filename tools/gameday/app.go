package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/client-go/dynamic"
)

const version = "0.1.0"

// Exit codes. Scripts (the agent evaluation, #26) rely on them.
const (
	exitOK      = 0
	exitError   = 1 // something failed; partial changes were rolled back where possible
	exitRefused = 2 // bad usage, or a safety check refused to start
	exitManual  = 3 // the fault needs a person to follow its procedure (see --manual)
)

const usage = `gameday runs one fault from chaos/faults against the lab cluster, for a bounded time.

Usage:
  gameday list                          the fault library
  gameday start <id> [--duration 10m]   apply a fault, hold it, remove it (blocks; Ctrl-C ends early)
  gameday start <id> --manual           record a timeline while you follow a manual procedure
  gameday stop <id>                     remove a running fault's objects
  gameday stop --all                    remove every fault object in namespace boutique
  gameday status                        fault objects in the cluster now

Flags (every command):
  -o text|json        output format on stdout (logs are JSON on stderr)
  --repo DIR          repository root (default: found by walking up from the current directory)
  --kubeconfig FILE   kubeconfig (default: $KUBECONFIG or ~/.kube/config)
  --context NAME      kubeconfig context (default: the current context)

Exit codes: 0 ok, 1 error, 2 usage error or refused by a safety check, 3 manual procedure needed.
`

// app holds everything a command needs, so tests can swap the cluster, the clock and stdin.
type app struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	log            *slog.Logger
	now            func() time.Time
	getwd          func() (string, error)
	newClient      func(kubeconfig, kubeContext string) (dynamic.Interface, string, error)

	minDuration    time.Duration
	pollInterval   time.Duration
	cleanupTimeout time.Duration
}

func newApp(stdout, stderr io.Writer, stdin io.Reader) *app {
	return &app{
		stdout:         stdout,
		stderr:         stderr,
		stdin:          stdin,
		log:            newLogger(stderr),
		now:            time.Now,
		getwd:          os.Getwd,
		newClient:      newKubeClient,
		minDuration:    30 * time.Second,
		pollInterval:   5 * time.Second,
		cleanupTimeout: 3 * time.Minute,
	}
}

// cliError carries an exit code with an error.
type cliError struct {
	code int
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

func refused(format string, args ...any) error {
	return &cliError{code: exitRefused, err: fmt.Errorf(format, args...)}
}

// options are the flags every command accepts.
type options struct {
	output     string
	repo       string
	kubeconfig string
	context    string
}

func (o *options) register(fs *flag.FlagSet) {
	fs.StringVar(&o.output, "o", "text", "output format: text or json")
	fs.StringVar(&o.repo, "repo", "", "repository root")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file")
	fs.StringVar(&o.context, "context", "", "kubeconfig context")
}

func (a *app) run(ctx context.Context, args []string) int {
	err := a.dispatch(ctx, args)
	if err == nil {
		return exitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	code := exitError
	var ce *cliError
	if errors.As(err, &ce) {
		code = ce.code
	}
	if code != exitManual {
		a.log.Error(err.Error(), "event", "error", "exit_code", code)
	}
	return code
}

func (a *app) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		_, _ = io.WriteString(a.stderr, usage)
		return &cliError{code: exitRefused, err: errors.New("no command")}
	}
	cmd, rest := args[0], args[1:]
	var opts options
	fs := flag.NewFlagSet("gameday "+cmd, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.Usage = func() { _, _ = io.WriteString(a.stderr, usage) }
	opts.register(fs)

	switch cmd {
	case "list":
		if _, err := parseArgs(fs, rest, 0); err != nil {
			return err
		}
		return a.withLibrary(opts, func(lib *library) error { return a.list(opts, lib) })
	case "start":
		var dur time.Duration
		var manual bool
		fs.DurationVar(&dur, "duration", 0, "how long to hold the fault (default: the fault's own; max 30m)")
		fs.BoolVar(&manual, "manual", false, "record a timeline while you follow a manual procedure")
		pos, err := parseArgs(fs, rest, 1)
		if err != nil {
			return err
		}
		return a.withLibrary(opts, func(lib *library) error {
			f, err := lib.get(pos[0])
			if err != nil {
				return refused("%w", err)
			}
			return a.start(ctx, opts, f, dur, manual)
		})
	case "stop":
		var all bool
		fs.BoolVar(&all, "all", false, "remove every fault object in namespace boutique")
		pos, err := parseArgs(fs, rest, -1)
		if err != nil {
			return err
		}
		switch {
		case all && len(pos) == 0:
			return a.stop(ctx, opts, "")
		case !all && len(pos) == 1:
			if !faultIDPattern(pos[0]) {
				return refused("%q is not a fault id (like F01-currency-abort)", pos[0])
			}
			return a.stop(ctx, opts, pos[0])
		default:
			return refused("usage: gameday stop <id> | gameday stop --all")
		}
	case "status":
		if _, err := parseArgs(fs, rest, 0); err != nil {
			return err
		}
		return a.status(ctx, opts)
	case "version":
		_, err := fmt.Fprintln(a.stdout, "gameday", version)
		return err
	case "help", "-h", "--help":
		_, err := io.WriteString(a.stdout, usage)
		return err
	default:
		_, _ = io.WriteString(a.stderr, usage)
		return refused("unknown command %q", cmd)
	}
}

// parseArgs parses flags placed anywhere among the arguments and checks the number of
// positional arguments (-1: any).
func parseArgs(fs *flag.FlagSet, args []string, want int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &cliError{code: exitRefused, err: err}
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if want >= 0 && len(pos) != want {
		return nil, refused("%s: expected %d argument(s), got %d", fs.Name(), want, len(pos))
	}
	if o := fs.Lookup("o"); o != nil && o.Value.String() != "text" && o.Value.String() != "json" {
		return nil, refused("-o must be text or json")
	}
	return pos, nil
}

func faultIDPattern(id string) bool {
	if len(id) < 5 || id[0] != 'F' || id[3] != '-' {
		return false
	}
	for _, r := range id[1:3] {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range id[4:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func (a *app) withLibrary(opts options, fn func(*library) error) error {
	repo, err := a.repoRoot(opts.repo)
	if err != nil {
		return err
	}
	lib, err := loadLibrary(repo)
	if err != nil {
		return err
	}
	return fn(lib)
}

// repoRoot returns --repo, or the nearest directory at or above the working directory that
// holds chaos/faults.
func (a *app) repoRoot(flagValue string) (string, error) {
	if flagValue != "" {
		if _, err := os.Stat(filepath.Join(flagValue, faultsDir)); err != nil {
			return "", refused("--repo %s: no %s there", flagValue, faultsDir)
		}
		return flagValue, nil
	}
	dir, err := a.getwd()
	if err != nil {
		return "", err
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, faultsDir)); err == nil && st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", refused("no %s above the current directory; run from the repository or pass --repo", faultsDir)
		}
		dir = parent
	}
}

func (a *app) cluster(opts options) (cluster, string, error) {
	dc, name, err := a.newClient(opts.kubeconfig, opts.context)
	if err != nil {
		return cluster{}, "", err
	}
	return cluster{dc: dc}, name, nil
}

type faultSummary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Category string `json:"category"`
	GameDay  bool   `json:"gameDay"`
	Mode     string `json:"mode"`
	Duration string `json:"duration"`
	Target   string `json:"target"`
}

// list prints the library without the expected answers, so a terminal shared during a game
// day gives nothing away beyond the titles.
func (a *app) list(opts options, lib *library) error {
	var out []faultSummary
	for _, f := range lib.sorted() {
		out = append(out, faultSummary{
			ID: f.ID, Title: f.Title, Category: f.Category, GameDay: f.GameDay,
			Mode: f.mode(), Duration: f.Duration, Target: f.Target.Kind + "/" + f.Target.Name,
		})
	}
	if opts.output == "json" {
		return a.printJSON(out)
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tCATEGORY\tGAME DAY\tHOW\tDURATION\tTITLE")
	for _, s := range out {
		gd := ""
		if s.GameDay {
			gd = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Category, gd, s.Mode, s.Duration, s.Title)
	}
	return w.Flush()
}

func (a *app) printJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.stdout, string(data))
	return err
}

func (a *app) printf(opts options, format string, args ...any) {
	if opts.output == "text" {
		_, _ = fmt.Fprintf(a.stdout, format+"\n", args...)
	}
}

// durationText formats whole minutes as "10m" (Chaos Mesh and people both read that), and
// anything else the Go way.
func durationText(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

func joinRefs(refs []string) string { return strings.Join(refs, ", ") }
