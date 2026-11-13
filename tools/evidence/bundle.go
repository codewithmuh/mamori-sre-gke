package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// schemaVersion names the layout of bundle.json. Change it whenever a field is renamed, removed or
// changes meaning; adding a field keeps it.
const schemaVersion = "mamori.evidence/v1"

const (
	statusCollected    = "collected"
	statusNotCollected = "not collected"

	// Inputs larger than this are refused rather than read into memory. An SBOM of this repository
	// is well under 1 MiB.
	maxInputBytes = 64 << 20

	// A reason is one short line written by the workflow; anything longer is cut.
	maxReasonChars = 300

	// Lists in the bundle stop here, so a bad week can't produce a bundle nobody can read. Counts
	// always cover everything.
	maxListItems = 50
)

// Input file names. The workflow writes exactly these into the inputs directory; for an input it
// could not collect it writes <name>.reason instead, holding one line that says why.
const (
	inputCIRuns      = "ci-runs.json"
	inputTrivy       = "trivy.json"
	inputGitleaks    = "gitleaks.json"
	inputSBOM        = "sbom.spdx.json"
	inputDrift       = "terraform-drift.json"
	inputGKECluster  = "gke-cluster.json"
	inputNodes       = "nodes.json"
	inputConstraints = "gatekeeper-constraints.json"
	inputArgoApps    = "argocd-applications.json"
	inputSLOStatus   = "slo-status.json"
	reasonSuffix     = ".reason"
)

// bundle is bundle.json. Field order here is the order in the file.
type bundle struct {
	SchemaVersion string   `json:"schemaVersion"`
	GeneratedAt   string   `json:"generatedAt"`
	Source        source   `json:"source"`
	Overview      overview `json:"overview"`
	Sections      sections `json:"sections"`
}

// source says which run produced the bundle, so a reader can find its log and its commit.
type source struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Ref        string `json:"ref"`
	Workflow   string `json:"workflow"`
	Event      string `json:"event"`
	RunID      string `json:"runId"`
	RunAttempt string `json:"runAttempt"`
	RunURL     string `json:"runUrl"`
}

type overview struct {
	Sections     int `json:"sections"`
	Collected    int `json:"collected"`
	NotCollected int `json:"notCollected"`
	// Keys of the collected sections whose content needs a look (a failing workflow, drift, a
	// violation, an exhausted error budget, ...).
	Attention []string `json:"attention"`
}

type sections struct {
	CI             section[ciData]         `json:"ci"`
	Trivy          section[trivyData]      `json:"trivy"`
	Gitleaks       section[gitleaksData]   `json:"gitleaks"`
	SBOM           section[sbomData]       `json:"sbom"`
	TerraformDrift section[driftData]      `json:"terraformDrift"`
	Cluster        section[clusterData]    `json:"cluster"`
	Gatekeeper     section[gatekeeperData] `json:"gatekeeper"`
	ArgoCD         section[argoCDData]     `json:"argocd"`
	SLOs           section[sloData]        `json:"slos"`
}

// section is one area of evidence. A section that could not be collected keeps its place, with
// status "not collected" and the reason, so a gap is visible instead of silently missing.
type section[T any] struct {
	Title     string     `json:"title"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	Attention bool       `json:"attention"`
	Headline  string     `json:"headline"`
	Inputs    []inputRef `json:"inputs"`
	Data      *T         `json:"data,omitempty"`
}

// inputRef records one input file, with its digest so it can be checked against the copy in the
// bundle's inputs/ directory.
type inputRef struct {
	File   string `json:"file"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// input is a file read from the inputs directory, or the reason it is missing.
type input struct {
	ref  inputRef
	data []byte
}

func (in input) ok() bool { return in.ref.Status == statusCollected }

// inputDir reads input files by fixed name only, through an os.Root so that no name can reach
// outside the directory.
type inputDir struct {
	root *os.Root
}

func openInputDir(path string) (*inputDir, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open inputs directory: %w", err)
	}
	return &inputDir{root: root}, nil
}

func (d *inputDir) Close() error { return d.root.Close() }

// read returns the named input. A missing or unusable file is not an error: it comes back as
// "not collected" with the reason the workflow recorded, or a reason of its own.
func (d *inputDir) read(name string) input {
	ref := inputRef{File: name}
	data, err := d.readFile(name)
	switch {
	case err == nil && len(data) == 0:
		ref.Status, ref.Reason = statusNotCollected, "input file is empty"
	case err == nil:
		sum := sha256.Sum256(data)
		ref.Status, ref.SHA256, ref.Bytes = statusCollected, hex.EncodeToString(sum[:]), int64(len(data))
		return input{ref: ref, data: data}
	case errors.Is(err, fs.ErrNotExist):
		ref.Status, ref.Reason = statusNotCollected, d.reason(name)
	default:
		ref.Status, ref.Reason = statusNotCollected, "could not read input: "+err.Error()
	}
	return input{ref: ref}
}

func (d *inputDir) readFile(name string) ([]byte, error) {
	f, err := d.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInputBytes {
		return nil, fmt.Errorf("larger than %d MiB", maxInputBytes>>20)
	}
	return data, nil
}

// reason returns the first line of <name>.reason, or a generic reason when the workflow left none.
func (d *inputDir) reason(name string) string {
	data, err := d.readFile(name + reasonSuffix)
	if err != nil {
		return "no input file and no reason recorded"
	}
	line, _, _ := bufio.NewReader(bytes.NewReader(data)).ReadLine()
	text := strings.TrimSpace(string(line))
	if text == "" {
		return "no input file; the reason file is empty"
	}
	return truncate(text, maxReasonChars)
}

// newSection builds a section from its main input, using parse for the content. parse returns
// the data, whether it needs attention, and a one-line headline.
func newSection[T any](title string, in input, parse func([]byte) (*T, bool, string, error)) section[T] {
	s := section[T]{Title: title, Inputs: []inputRef{in.ref}}
	if !in.ok() {
		return s.notCollected(in.ref.Reason)
	}
	data, attention, headline, err := parse(in.data)
	if err != nil {
		return s.notCollected(fmt.Sprintf("%s is not valid: %v", in.ref.File, err))
	}
	s.Status, s.Data, s.Attention, s.Headline = statusCollected, data, attention, headline
	return s
}

func (s section[T]) notCollected(reason string) section[T] {
	s.Status, s.Reason, s.Attention, s.Data = statusNotCollected, reason, false, nil
	s.Headline = statusNotCollected + ": " + reason
	return s
}

// assemble reads every input and builds the bundle. It never fails because of an input: missing or
// invalid inputs become "not collected" sections.
func assemble(dir *inputDir, src source, now time.Time) bundle {
	b := bundle{
		SchemaVersion: schemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Source:        src,
		Sections: sections{
			CI:             newSection("CI on main", dir.read(inputCIRuns), parseCIRuns),
			Trivy:          newSection("Vulnerabilities and misconfigurations (Trivy)", dir.read(inputTrivy), parseTrivy),
			Gitleaks:       newSection("Secrets in Git history (gitleaks)", dir.read(inputGitleaks), parseGitleaks),
			SBOM:           newSection("Software bill of materials (syft)", dir.read(inputSBOM), parseSBOM),
			TerraformDrift: newSection("Terraform drift (infra/envs/lab)", dir.read(inputDrift), parseDrift),
			Cluster:        clusterSection(dir.read(inputGKECluster), dir.read(inputNodes)),
			Gatekeeper:     newSection("Gatekeeper audit", dir.read(inputConstraints), parseConstraints),
			ArgoCD:         newSection("Argo CD applications", dir.read(inputArgoApps), parseArgoApps),
			SLOs:           newSection("SLOs and error budgets", dir.read(inputSLOStatus), parseSLOStatus),
		},
	}
	b.Overview = summarise(b.Sections)
	return b
}

// sectionInfo is what the overview and summary.md need from any section.
type sectionInfo struct {
	key       string
	title     string
	status    string
	attention bool
	headline  string
	inputs    []inputRef
}

func info[T any](key string, s section[T]) sectionInfo {
	return sectionInfo{key: key, title: s.Title, status: s.Status, attention: s.Attention, headline: s.Headline, inputs: s.Inputs}
}

// list returns the sections in bundle order, with the JSON keys they have in bundle.json.
func (s sections) list() []sectionInfo {
	return []sectionInfo{
		info("ci", s.CI),
		info("trivy", s.Trivy),
		info("gitleaks", s.Gitleaks),
		info("sbom", s.SBOM),
		info("terraformDrift", s.TerraformDrift),
		info("cluster", s.Cluster),
		info("gatekeeper", s.Gatekeeper),
		info("argocd", s.ArgoCD),
		info("slos", s.SLOs),
	}
}

func summarise(s sections) overview {
	o := overview{Attention: []string{}}
	for _, si := range s.list() {
		o.Sections++
		if si.status == statusCollected {
			o.Collected++
		} else {
			o.NotCollected++
		}
		if si.attention {
			o.Attention = append(o.Attention, si.key)
		}
	}
	return o
}

// decode parses JSON strictly enough to catch a wrong file (trailing data), but ignores unknown
// fields, because the tools that write the inputs add fields over time.
func decode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
