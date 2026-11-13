package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoCopy makes a throwaway repository root holding the fixture inputs.
func repoCopy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"slo/slos.yaml", "slo/cuj-map.yaml"} {
		b, err := os.ReadFile(filepath.Join(fixtureRoot, f))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	code := run(args, &stderr)
	return code, stderr.String()
}

func TestRunWriteAndCheck(t *testing.T) {
	root := repoCopy(t)
	args := []string{"-root", root, "-cuj-map", "slo/cuj-map.yaml"}

	if code, out := runCLI(t, append(args, "-check")...); code != exitOutdated || !strings.Contains(out, `"file missing"`) {
		t.Fatalf("check before generating: exit %d\n%s", code, out)
	}
	if code, out := runCLI(t, args...); code != exitOK || !strings.Contains(out, `"file written"`) || !strings.Contains(out, "wishlist") {
		t.Fatalf("generate: exit %d\n%s", code, out)
	}
	if code, out := runCLI(t, args...); code != exitOK || strings.Contains(out, `"file written"`) {
		t.Fatalf("second generate should change nothing: exit %d\n%s", code, out)
	}
	if code, out := runCLI(t, append(args, "-check")...); code != exitOK {
		t.Fatalf("check after generating: exit %d\n%s", code, out)
	}

	// A hand edit makes the check fail; regenerating repairs it.
	lab := filepath.Join(root, filepath.FromSlash(pathLabTF))
	if err := os.WriteFile(lab, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runCLI(t, append(args, "-check")...); code != exitOutdated || !strings.Contains(out, `"file stale"`) {
		t.Fatalf("check after a hand edit: exit %d\n%s", code, out)
	}

	// A leftover file in a generated directory is reported, then removed.
	extra := filepath.Join(root, "deploy", "slo", "old-rules.yaml")
	if err := os.WriteFile(extra, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runCLI(t, append(args, "-check")...); code != exitOutdated || !strings.Contains(out, `"file extra"`) {
		t.Fatalf("check with a leftover: exit %d\n%s", code, out)
	}
	if code, out := runCLI(t, args...); code != exitOK || !strings.Contains(out, `"file removed"`) {
		t.Fatalf("generate with a leftover: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Errorf("leftover still there: %v", err)
	}
	if code, out := runCLI(t, append(args, "-check")...); code != exitOK {
		t.Fatalf("final check: exit %d\n%s", code, out)
	}
}

func TestRunErrors(t *testing.T) {
	root := repoCopy(t)
	cases := map[string][]string{
		"unknown flag":   {"-nope"},
		"extra argument": {"-root", root, "-cuj-map", "slo/cuj-map.yaml", "extra"},
		"missing map":    {"-root", root, "-cuj-map", "slo/none.yaml"},
		"missing spec":   {"-root", root, "-cuj-map", "slo/cuj-map.yaml", "-slos", "slo/none.yaml"},
	}
	for name, args := range cases {
		if code, out := runCLI(t, args...); code != exitError {
			t.Errorf("%s: exit %d, want %d\n%s", name, code, exitError, out)
		}
	}

	// A map that lacks a journey of the spec is an error, not a silent gap.
	short := filepath.Join(root, "slo", "short.yaml")
	if err := os.WriteFile(short, []byte("apiVersion: mamori/v1\nkind: CUJMap\njourneys:\n  - {id: browse, critical: [frontend]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runCLI(t, "-root", root, "-cuj-map", "slo/short.yaml"); code != exitError || !strings.Contains(out, "not in the CUJ map") {
		t.Errorf("short map: exit %d\n%s", code, out)
	}
}

func TestSyncWriteError(t *testing.T) {
	root := t.TempDir()
	// A file where a directory is needed makes the write fail.
	if err := os.WriteFile(filepath.Join(root, "deploy"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(root, []File{{Path: "deploy/slo/x.yaml", Content: []byte("y")}}, false); err == nil {
		t.Error("want an error")
	}
}

func TestCheckTemplateSafe(t *testing.T) {
	if err := checkTemplateSafe(File{Path: "a.tf.json", Content: []byte(`{"x": "100%{"}`)}); err == nil {
		t.Error("want an error for %{ in Terraform JSON")
	}
	if err := checkTemplateSafe(File{Path: "a.yaml", Content: []byte(`%{`)}); err != nil {
		t.Errorf("YAML is not a Terraform template: %v", err)
	}
}

func TestLogger(t *testing.T) {
	var b bytes.Buffer
	log := newLogger(&b)
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	log.WithGroup("g").Info("grouped", "k", "v")
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	want := []string{"INFO", "WARNING", "ERROR", "INFO"}
	for i, l := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["severity"] != want[i] || rec["message"] == nil || rec["time"] != nil {
			t.Errorf("line %d = %v", i, rec)
		}
	}
	if severity(slog.LevelDebug) != "INFO" {
		t.Error("debug should map to INFO")
	}
}
