package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The contract's game-day faults: #20 plans the game days around exactly these ids.
var gameDayIDs = []string{
	"F01-currency-abort",
	"F02-productcatalog-latency",
	"F03-redis-cart-pod-kill",
	"F04-checkout-bad-config",
	"F05-node-preemption",
	"F06-recommendation-cpu-stress",
}

// TestRepoLibrary checks every file in chaos/faults against chaos/fault.schema.json and the
// rules a schema cannot express (loadLibrary), then the library as a whole.
func TestRepoLibrary(t *testing.T) {
	lib, err := loadLibrary(realRepo)
	if err != nil {
		t.Fatalf("the fault library does not load:\n%v", err)
	}
	faults := lib.sorted()

	if n := len(faults); n < 20 || n > 30 {
		t.Errorf("library has %d faults, want 20-30", n)
	}
	for _, f := range faults {
		want := slices.Contains(gameDayIDs, f.ID)
		if f.GameDay != want {
			t.Errorf("%s: gameDay = %v, want %v (game-day faults are exactly %v)", f.ID, f.GameDay, want, gameDayIDs)
		}
	}
	for _, id := range gameDayIDs {
		if _, err := lib.get(id); err != nil {
			t.Errorf("game-day fault missing: %v", err)
		}
	}

	categories := map[string]bool{}
	for _, f := range faults {
		categories[f.Category] = true
	}
	for _, c := range []string{"istio-abort", "istio-delay", "pod-failure", "network", "stress", "bad-deploy", "config", "node"} {
		if !categories[c] {
			t.Errorf("no fault in category %s", c)
		}
	}
}

// TestRepoManifests checks every object in chaos/faults/manifests: labelled with its own
// fault id, in boutique, an allowed kind; and that no manifest file is orphaned.
func TestRepoManifests(t *testing.T) {
	lib, err := loadLibrary(realRepo)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, f := range lib.sorted() {
		if f.manual() {
			if f.Manifest != "" || len(f.objects) != 0 {
				t.Errorf("%s: a manual fault must not have objects", f.ID)
			}
			continue
		}
		used[filepath.Base(f.Manifest)] = true
		if f.Manifest != "manifests/"+f.ID+".yaml" {
			t.Errorf("%s: manifest should be manifests/%s.yaml, got %s", f.ID, f.ID, f.Manifest)
		}
		if len(f.objects) == 0 {
			t.Errorf("%s: no objects", f.ID)
		}
		for _, o := range f.objects {
			if o.GetNamespace() != faultNamespace {
				t.Errorf("%s: %s is in %q", f.ID, objectRef(o), o.GetNamespace())
			}
			l := o.GetLabels()
			if l[labelFault] != "true" || l[labelFaultID] != f.ID {
				t.Errorf("%s: %s labels %v", f.ID, objectRef(o), l)
			}
			if _, ok := faultKinds[o.GroupVersionKind()]; !ok {
				t.Errorf("%s: %s has a kind gameday refuses", f.ID, objectRef(o))
			}
			if o.GroupVersionKind().Kind == "Deployment" {
				// The pod template carries the labels too, so the pods are recognisable.
				tl, _, _ := unstructured.NestedStringMap(o.Object, "spec", "template", "metadata", "labels")
				if tl[labelFault] != "true" || tl[labelFaultID] != f.ID {
					t.Errorf("%s: pod template labels %v", f.ID, tl)
				}
			}
		}
		if f.Expected.CorrectAction.Type == "remove-fault" && !strings.HasPrefix(f.Expected.CorrectAction.Target, strings.ToLower(f.objects[0].GetKind())+"/") {
			t.Errorf("%s: correct action %s does not name its own object", f.ID, f.Expected.CorrectAction.Target)
		}
	}
	entries, err := os.ReadDir(filepath.Join(realRepo, faultsDir, "manifests"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !used[e.Name()] {
			t.Errorf("manifests/%s is not used by any fault", e.Name())
		}
	}
}

// TestRepoManualFaults: the faults that cannot be manifests are exactly the ones expected,
// and their procedures say how to undo them.
func TestRepoManualFaults(t *testing.T) {
	lib, err := loadLibrary(realRepo)
	if err != nil {
		t.Fatal(err)
	}
	var manual []string
	for _, f := range lib.sorted() {
		if f.manual() {
			manual = append(manual, f.ID)
		}
	}
	want := []string{"F05-node-preemption", "F25-checkout-bad-deploy-git", "F26-node-drain"}
	if !slices.Equal(manual, want) {
		t.Errorf("manual faults = %v, want %v", manual, want)
	}
}
