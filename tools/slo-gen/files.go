package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// File is one generated file; Path is slash-separated and relative to the
// repository root.
type File struct {
	Path    string
	Content []byte
}

// ownedDirs are directories whose every file is generated. A file there that
// this run did not produce is stale (a journey was removed, a file renamed).
var ownedDirs = []string{"deploy/slo", "slo/generated"}

// Render produces every output file, sorted by path.
func Render(m *Model) ([]File, error) {
	files, err := m.renderRules()
	if err != nil {
		return nil, err
	}
	lab, err := m.renderLab()
	if err != nil {
		return nil, err
	}
	dd, err := m.renderDatadog()
	if err != nil {
		return nil, err
	}
	files = append(files, lab, dd, m.renderCatalog())
	for _, f := range files {
		if err := checkTemplateSafe(f); err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, k int) bool { return files[i].Path < files[k].Path })
	return files, nil
}

// checkTemplateSafe guards the Terraform files: in Terraform's JSON syntax
// every string is a template, so "%{" starts a directive. "${" is how the
// generator references other resources on purpose.
func checkTemplateSafe(f File) error {
	if strings.HasSuffix(f.Path, ".tf.json") && bytes.Contains(f.Content, []byte("%{")) {
		return fmt.Errorf("%s: generated text contains %%{, which Terraform would read as a template directive", f.Path)
	}
	return nil
}

// Status of one file after a write or check.
type Status string

// File statuses.
const (
	StatusUnchanged Status = "unchanged"
	StatusWritten   Status = "written"
	StatusRemoved   Status = "removed"
	StatusStale     Status = "stale"
	StatusMissing   Status = "missing"
	StatusExtra     Status = "extra"
)

// Result is what happened to one path.
type Result struct {
	Path   string
	Status Status
}

// Sync writes the files under root (only those whose content changed) and
// removes leftover files in the owned directories. With check set it changes
// nothing and reports what is out of date instead.
func Sync(root string, files []File, check bool) ([]Result, error) {
	var results []Result
	want := map[string]bool{}
	for _, f := range files {
		want[f.Path] = true
		full := filepath.Join(root, filepath.FromSlash(f.Path))
		old, err := os.ReadFile(full) //nolint:gosec // paths are the generator's own
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if check {
				results = append(results, Result{f.Path, StatusMissing})
				continue
			}
		case err != nil:
			return nil, err
		case bytes.Equal(old, f.Content):
			results = append(results, Result{f.Path, StatusUnchanged})
			continue
		case check:
			results = append(results, Result{f.Path, StatusStale})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil { //nolint:gosec // repository files are world-readable
			return nil, err
		}
		results = append(results, Result{f.Path, StatusWritten})
	}
	for _, dir := range ownedDirs {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			p := path.Join(dir, e.Name())
			if e.IsDir() || want[p] {
				continue
			}
			if check {
				results = append(results, Result{p, StatusExtra})
				continue
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(p))); err != nil {
				return nil, err
			}
			results = append(results, Result{p, StatusRemoved})
		}
	}
	return results, nil
}

// OutOfDate reports whether a check found anything to regenerate.
func OutOfDate(results []Result) bool {
	for _, r := range results {
		switch r.Status {
		case StatusStale, StatusMissing, StatusExtra:
			return true
		}
	}
	return false
}
