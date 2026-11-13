// Command slo-gen turns the journey SLO spec (slo/slos.yaml) and the CUJ map
// (slo/cuj-map.yaml) into Managed Prometheus recording rules, Cloud Monitoring
// SLOs and burn-rate alerts, Datadog SLOs and monitors, and a catalog.
//
//	slo-gen [-root .] [-slos slo/slos.yaml] [-cuj-map slo/cuj-map.yaml] [-check]
//
// Without -check it writes the files that changed. With -check it writes
// nothing and exits 1 if any generated file is missing, stale or left over.
// Exit status 2 means bad flags or invalid input.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	exitOK       = 0
	exitOutdated = 1
	exitError    = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("slo-gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root; the other paths are relative to it")
	slosPath := fs.String("slos", "slo/slos.yaml", "SLO spec")
	mapPath := fs.String("cuj-map", "slo/cuj-map.yaml", "CUJ map from cuj-discovery (slo/cuj-map.example.yaml until the first discovery run)")
	check := fs.Bool("check", false, "write nothing; exit 1 if generated files are missing, stale or left over")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	log := newLogger(stderr)
	if fs.NArg() > 0 {
		log.Error("unexpected arguments", "args", fs.Args())
		return exitError
	}

	files, err := generate(*root, Inputs{SLOs: filepath.ToSlash(*slosPath), CUJMap: filepath.ToSlash(*mapPath)}, log)
	if err != nil {
		log.Error("generation failed", "error", err.Error())
		return exitError
	}
	results, err := Sync(*root, files, *check)
	if err != nil {
		log.Error("writing files failed", "error", err.Error())
		return exitError
	}
	for _, r := range results {
		level := slog.LevelInfo
		if *check && r.Status != StatusUnchanged {
			level = slog.LevelError
		}
		log.Log(context.Background(), level, "file "+string(r.Status), "path", r.Path)
	}
	if *check && OutOfDate(results) {
		log.Error("generated SLO files are out of date; run `make slo-generate` and commit the result",
			"slos", *slosPath, "cuj_map", *mapPath)
		return exitOutdated
	}
	log.Info("done", "files", len(results), "check", *check)
	return exitOK
}

func generate(root string, in Inputs, log *slog.Logger) ([]File, error) {
	spec, err := LoadSpec(filepath.Join(root, filepath.FromSlash(in.SLOs)))
	if err != nil {
		return nil, err
	}
	cujMap, err := LoadCUJMap(filepath.Join(root, filepath.FromSlash(in.CUJMap)))
	if err != nil {
		return nil, err
	}
	model, warnings, err := BuildModel(spec, cujMap, in)
	if err != nil {
		return nil, fmt.Errorf("%s + %s: %w", in.SLOs, in.CUJMap, err)
	}
	for _, w := range warnings {
		log.Warn(w)
	}
	return Render(model)
}

// newLogger writes one JSON object per line, with Cloud Logging's field names
// like the other tools (severity, message).
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				return slog.Attr{} // a CLI's output is read live; timestamps only add noise to diffs of logs
			case slog.LevelKey:
				level, _ := a.Value.Any().(slog.Level)
				return slog.String("severity", severity(level))
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	}))
}

func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	default:
		return "INFO"
	}
}
