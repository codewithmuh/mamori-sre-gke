package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// go test -run TestGolden -update rewrites testdata/golden from testdata/inputs. Review the diff:
// it is the schema.
var update = flag.Bool("update", false, "rewrite the golden files")

var fixedNow = time.Date(2026, 11, 16, 0, 20, 0, 0, time.UTC)

func githubEnv(key string) string {
	return map[string]string{
		"GITHUB_REPOSITORY":  "codewithmuh/mamori-sre-gke",
		"GITHUB_SHA":         "0123456789abcdef0123456789abcdef01234567",
		"GITHUB_REF":         "refs/heads/main",
		"GITHUB_WORKFLOW":    "evidence",
		"GITHUB_EVENT_NAME":  "schedule",
		"GITHUB_RUN_ID":      "9100000005",
		"GITHUB_RUN_ATTEMPT": "1",
		"GITHUB_SERVER_URL":  "https://github.com",
	}[key]
}

func fixedClock() time.Time { return fixedNow }

func TestGolden(t *testing.T) {
	out := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"-inputs", "testdata/inputs", "-out", out}, githubEnv, &stderr, fixedClock); code != 0 {
		t.Fatalf("run = %d, stderr:\n%s", code, stderr.String())
	}
	for _, name := range []string{bundleFile, summaryFile} {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", "golden", name)
		if *update {
			if err := os.WriteFile(golden, got, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s; run go test -run TestGolden -update and review the diff\n--- got:\n%s", name, golden, got)
		}
	}
}

func TestEverySectionHasAPlaceWhenNothingWasCollected(t *testing.T) {
	in := t.TempDir()
	// The workflow records why the cloud inputs are missing; the others have no reason file.
	for name, reason := range map[string]string{
		inputDrift:      "Google Cloud not set up: repository variable GCP_WIF_PROVIDER is empty\nsecond line is ignored",
		inputGKECluster: "  cluster mamori not found in asia-northeast1-a (lab is down)  ",
		inputSLOStatus:  "",
	} {
		writeFile(t, in, name+reasonSuffix, reason)
	}
	writeFile(t, in, inputTrivy, "")

	b := runAndRead(t, in)

	if b.Overview.Collected != 0 || b.Overview.NotCollected != 9 || b.Overview.Sections != 9 {
		t.Errorf("overview = %+v, want 0 collected and 9 not collected", b.Overview)
	}
	if len(b.Overview.Attention) != 0 {
		t.Errorf("attention = %v, want none: nothing collected can't need a look", b.Overview.Attention)
	}
	want := map[string]string{
		"ci":             "no input file and no reason recorded",
		"trivy":          "input file is empty",
		"terraformDrift": "Google Cloud not set up: repository variable GCP_WIF_PROVIDER is empty",
		"cluster":        "cluster mamori not found in asia-northeast1-a (lab is down)",
		"slos":           "no input file; the reason file is empty",
	}
	for _, si := range b.Sections.list() {
		if si.status != statusNotCollected {
			t.Errorf("%s: status %q, want %q", si.key, si.status, statusNotCollected)
		}
		if !strings.HasPrefix(si.headline, "not collected: ") {
			t.Errorf("%s: headline %q, want it to start with \"not collected: \"", si.key, si.headline)
		}
		if r, ok := want[si.key]; ok && si.headline != "not collected: "+r {
			t.Errorf("%s: headline %q, want %q", si.key, si.headline, "not collected: "+r)
		}
	}
	if n := len(b.Sections.Cluster.Inputs); n != 2 {
		t.Errorf("cluster inputs = %d, want 2 (cluster and nodes)", n)
	}
}

func TestInvalidInputIsNotCollectedWithTheParseError(t *testing.T) {
	in := t.TempDir()
	writeFile(t, in, inputCIRuns, "{not json")
	writeFile(t, in, inputArgoApps, `{"items": []} {"items": []}`)
	writeFile(t, in, inputSLOStatus, `{"slos": [{"name": "x", "goal": 99.5}]}`)

	b := runAndRead(t, in)

	if s := b.Sections.CI; s.Status != statusNotCollected || !strings.Contains(s.Reason, "ci-runs.json is not valid") {
		t.Errorf("ci = %q / %q", s.Status, s.Reason)
	}
	if s := b.Sections.CI; s.Inputs[0].Status != statusCollected || len(s.Inputs[0].SHA256) != 64 {
		t.Errorf("an unparsable file is still recorded with its digest: %+v", s.Inputs[0])
	}
	if s := b.Sections.ArgoCD; s.Status != statusNotCollected || !strings.Contains(s.Reason, "unexpected data after the JSON value") {
		t.Errorf("argocd = %q / %q", s.Status, s.Reason)
	}
	if s := b.Sections.SLOs; s.Status != statusNotCollected || !strings.Contains(s.Reason, "want a fraction") {
		t.Errorf("slos = %q / %q", s.Status, s.Reason)
	}
}

func TestOversizedInputIsRefused(t *testing.T) {
	in := t.TempDir()
	f, err := os.Create(filepath.Join(in, inputSBOM))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxInputBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	b := runAndRead(t, in)
	if s := b.Sections.SBOM; s.Status != statusNotCollected || !strings.Contains(s.Reason, "larger than 64 MiB") {
		t.Errorf("sbom = %q / %q", s.Status, s.Reason)
	}
}

func TestInputNamesCannotEscapeTheDirectory(t *testing.T) {
	parent := t.TempDir()
	in := filepath.Join(parent, "inputs")
	if err := os.Mkdir(in, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, parent, "secret.json", "{}")
	if err := os.Symlink(filepath.Join(parent, "secret.json"), filepath.Join(in, inputTrivy)); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	b := runAndRead(t, in)
	if s := b.Sections.Trivy; s.Status != statusNotCollected || !strings.Contains(s.Reason, "could not read input") {
		t.Errorf("a symlink out of the inputs directory must not be followed: %q / %q", s.Status, s.Reason)
	}
}

func TestRunFlags(t *testing.T) {
	noEnv := func(string) string { return "" }
	tests := []struct {
		name   string
		args   []string
		want   int
		stderr string
	}{
		{"missing both", nil, 2, "-inputs is required"},
		{"bad now", []string{"-inputs", "x", "-out", "y", "-now", "yesterday"}, 2, "-now"},
		{"extra argument", []string{"-inputs", "x", "-out", "y", "z"}, 2, "unexpected arguments: z"},
		{"unknown flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"help", []string{"-h"}, 0, "-inputs"},
		{"inputs missing on disk", []string{"-inputs", filepath.Join(t.TempDir(), "absent"), "-out", t.TempDir()}, 1, "cannot read inputs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(tc.args, noEnv, &stderr, fixedClock); got != tc.want {
				t.Errorf("run = %d, want %d; stderr:\n%s", got, tc.want, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("stderr does not mention %q:\n%s", tc.stderr, stderr.String())
			}
		})
	}
}

func TestRunFailsWhenItCannotWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	writeFile(t, filepath.Dir(blocker), "file", "not a directory")
	var stderr bytes.Buffer
	if got := run([]string{"-inputs", t.TempDir(), "-out", blocker}, githubEnv, &stderr, fixedClock); got != 1 {
		t.Fatalf("run = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "cannot write the bundle") {
		t.Errorf("stderr:\n%s", stderr.String())
	}
}

func TestSourceComesFromFlagsOverEnvironment(t *testing.T) {
	out := t.TempDir()
	args := []string{"-inputs", t.TempDir(), "-out", out, "-commit", "abc", "-now", "2026-01-02T03:04:05+09:00"}
	if code := run(args, githubEnv, &bytes.Buffer{}, fixedClock); code != 0 {
		t.Fatalf("run = %d", code)
	}
	b := readBundle(t, out)
	if b.Source.Commit != "abc" || b.Source.Repository != "codewithmuh/mamori-sre-gke" {
		t.Errorf("source = %+v", b.Source)
	}
	if b.Source.RunURL != "https://github.com/codewithmuh/mamori-sre-gke/actions/runs/9100000005" {
		t.Errorf("run url = %q", b.Source.RunURL)
	}
	if b.GeneratedAt != "2026-01-01T18:04:05Z" {
		t.Errorf("generatedAt = %q, want UTC", b.GeneratedAt)
	}
	if defaultRunURL(func(string) string { return "" }) != "" {
		t.Error("no run URL without GitHub's variables")
	}
}

func runAndRead(t *testing.T, inputs string) bundle {
	t.Helper()
	out := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"-inputs", inputs, "-out", out}, githubEnv, &stderr, fixedClock); code != 0 {
		t.Fatalf("run = %d, stderr:\n%s", code, stderr.String())
	}
	return readBundle(t, out)
}

func readBundle(t *testing.T, dir string) bundle {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, bundleFile))
	if err != nil {
		t.Fatal(err)
	}
	var b bundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != schemaVersion {
		t.Errorf("schemaVersion = %q", b.SchemaVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, summaryFile)); err != nil {
		t.Errorf("summary.md missing: %v", err)
	}
	return b
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
