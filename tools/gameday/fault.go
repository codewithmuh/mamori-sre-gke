package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// maxDuration caps every run, whatever the fault file or --duration says.
const maxDuration = 30 * time.Minute

// Paths inside the repository.
const (
	faultsDir  = "chaos/faults"
	schemaPath = "chaos/fault.schema.json"
)

// fault is one chaos/faults/<id>.yaml file. The JSON tags follow chaos/fault.schema.json.
type fault struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Category    string     `json:"category"`
	GameDay     bool       `json:"gameDay"`
	Manifest    string     `json:"manifest,omitempty"`
	Procedure   *procedure `json:"procedure,omitempty"`
	Target      target     `json:"target"`
	Duration    string     `json:"duration"`
	Description string     `json:"description"`
	Expected    expected   `json:"expected"`
	Notes       string     `json:"notes,omitempty"`

	duration time.Duration
	objects  []*unstructured.Unstructured
}

type procedure struct {
	Reason string   `json:"reason"`
	Inject []string `json:"inject"`
	Revert []string `json:"revert"`
}

type target struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

type expected struct {
	CUJs          []string  `json:"cujs"`
	SLIs          []string  `json:"slis"`
	RootCause     rootCause `json:"rootCause"`
	CorrectAction action    `json:"correctAction"`
	UnsafeActions []string  `json:"unsafeActions"`
}

type rootCause struct {
	Service            string   `json:"service"`
	Summary            string   `json:"summary"`
	AcceptableServices []string `json:"acceptableServices,omitempty"`
}

type action struct {
	Type   string `json:"type"`
	Target string `json:"target,omitempty"`
}

// manual reports whether a person has to inject this fault by following its procedure.
func (f *fault) manual() bool { return f.Procedure != nil }

func (f *fault) mode() string {
	if f.manual() {
		return "manual"
	}
	return "cli"
}

// library is every fault in chaos/faults, keyed by id.
type library struct {
	faults map[string]*fault
}

func (l *library) sorted() []*fault {
	out := make([]*fault, 0, len(l.faults))
	for _, f := range l.faults {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b *fault) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (l *library) get(id string) (*fault, error) {
	f, ok := l.faults[id]
	if !ok {
		return nil, fmt.Errorf("no fault %q in %s (see `gameday list`)", id, faultsDir)
	}
	return f, nil
}

// loadLibrary reads and checks every fault file under repo/chaos/faults. One bad file fails
// the whole load: a library that half-loads would hide mistakes until a game day. Ids are
// unique because each file must be named after its id.
func loadLibrary(repo string) (*library, error) {
	sch, err := compileSchema(filepath.Join(repo, schemaPath))
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(repo, faultsDir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no fault files in %s", filepath.Join(repo, faultsDir))
	}
	lib := &library{faults: map[string]*fault{}}
	var errs []error
	for _, file := range files {
		f, err := loadFault(sch, file)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lib.faults[f.ID] = f
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return lib, nil
}

func compileSchema(path string) (*jsonschema.Schema, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a path inside the repo, chosen by the operator
	if err != nil {
		return nil, fmt.Errorf("read fault schema: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("fault.schema.json", doc); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	sch, err := c.Compile("fault.schema.json")
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", path, err)
	}
	return sch, nil
}

// loadFault validates one fault file against the schema, then checks what a schema cannot:
// the file name, the duration cap, the manifest's objects and the correct action's target.
func loadFault(sch *jsonschema.Schema, file string) (*fault, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- a file under chaos/faults
	if err != nil {
		return nil, err
	}
	js, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := sch.Validate(doc); err != nil {
		return nil, fmt.Errorf("%s does not match %s: %w", file, schemaPath, err)
	}
	f := &fault{}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(f); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}

	var errs []error
	if want := f.ID + ".yaml"; filepath.Base(file) != want {
		errs = append(errs, fmt.Errorf("file name must be %s", want))
	}
	f.duration, err = time.ParseDuration(f.Duration)
	if err != nil || f.duration <= 0 || f.duration > maxDuration {
		errs = append(errs, fmt.Errorf("duration %q must be between 1m and %s", f.Duration, maxDuration))
	}
	if f.Manifest != "" {
		objs, err := loadManifest(f.ID, filepath.Join(filepath.Dir(file), filepath.FromSlash(f.Manifest)))
		if err != nil {
			errs = append(errs, err)
		}
		f.objects = objs
	}
	errs = append(errs, checkCorrectAction(f)...)
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return f, nil
}

func loadManifest(id, path string) ([]*unstructured.Unstructured, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the schema pins it to manifests/<id>.yaml
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	objs, err := parseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	var errs []error
	seen := map[string]bool{}
	for _, o := range objs {
		if err := checkFaultObject(id, o); err != nil {
			errs = append(errs, err)
		}
		if seen[objectRef(o)] {
			errs = append(errs, fmt.Errorf("%s appears twice", objectRef(o)))
		}
		seen[objectRef(o)] = true
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return objs, nil
}

// checkCorrectAction ties the answer key to the cluster: remove-fault must name one of this
// fault's own objects, and the other actions a Deployment of the shop.
func checkCorrectAction(f *fault) []error {
	a := f.Expected.CorrectAction
	switch a.Type {
	case "none":
		return nil
	case "remove-fault":
		if f.manual() {
			return []error{errors.New("a manual fault has no objects for remove-fault")}
		}
		for _, o := range f.objects {
			if objectRef(o) == a.Target {
				return nil
			}
		}
		return []error{fmt.Errorf("correctAction target %s is not an object in %s", a.Target, f.Manifest)}
	default:
		if !strings.HasPrefix(a.Target, "deployment/") {
			return []error{fmt.Errorf("correctAction %s must target deployment/<name>, got %s", a.Type, a.Target)}
		}
		return nil
	}
}
