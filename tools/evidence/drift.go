package main

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The lab's cluster. If even this would be created, the lab is down rather than drifted.
const clusterAddress = "google_container_cluster.mamori"

// expectedDrift lists, per resource type, the attributes that change outside Terraform as a
// matter of course: GKE upgrades the control plane and nodes on its own (REGULAR channel,
// auto-upgrade on), and the spend guard and `make resume` resize the node pool. A change to only
// these is recorded but doesn't need a look.
var expectedDrift = map[string][]string{
	"google_container_cluster":   {"master_version", "node_pool", "node_version"},
	"google_container_node_pool": {"instance_group_urls", "managed_instance_group_urls", "node_count", "version"},
}

// driftInput is terraform-drift.json, which the workflow builds with drift.jq from
// `terraform show -json` of a `plan -detailed-exitcode` run. Only addresses, actions and
// attribute names are kept: the plan's values can hold sensitive data, so they never leave the
// runner.
type driftInput struct {
	ExitCode *int             `json:"exitCode"`
	Changes  []resourceChange `json:"changes"`
	Drift    []driftEntry     `json:"drift"`
}

type resourceChange struct {
	Address string   `json:"address"`
	Actions []string `json:"actions"`
}

// driftEntry is one resource_drift entry. attributes are the top-level attributes whose refreshed
// value differs from state; relevantAttributes are those that Terraform lists in
// relevant_attributes, i.e. that this plan depends on. Without them (an older input) the entry
// counts as relevant.
type driftEntry struct {
	Address            string    `json:"address"`
	Actions            []string  `json:"actions"`
	Attributes         *[]string `json:"attributes"`
	RelevantAttributes *[]string `json:"relevantAttributes"`
}

type driftData struct {
	Root     string `json:"root"`
	ExitCode int    `json:"exitCode"`
	// "no changes", "changes", "outputs only" or "lab down".
	Result string `json:"result"`
	Counts struct {
		Create  int `json:"create"`
		Update  int `json:"update"`
		Replace int `json:"replace"`
		Delete  int `json:"delete"`
		Other   int `json:"other"`
	} `json:"counts"`
	// Planned changes, at most maxListItems.
	Changes          []resourceChange `json:"changes"`
	ChangesTruncated bool             `json:"changesTruncated"`
	// Objects that changed outside Terraform since the last apply (Terraform's resource_drift),
	// at most maxListItems, and how many of each verdict.
	ChangedOutsideTerraform []outsideChange `json:"changedOutsideTerraform"`
	OutsideCounts           struct {
		Relevant   int `json:"relevant"`
		Expected   int `json:"expected"`
		Irrelevant int `json:"irrelevant"`
	} `json:"outsideCounts"`
}

type outsideChange struct {
	Address            string   `json:"address"`
	Actions            []string `json:"actions"`
	Attributes         []string `json:"attributes"`
	RelevantAttributes []string `json:"relevantAttributes"`
	// "relevant": the plan depends on a change that GKE or the spend guard wouldn't make, or the
	// object is gone (needs a look); "expected": only changes they make on their own;
	// "irrelevant": nothing in this plan depends on the change.
	Verdict string `json:"verdict"`
}

func parseDrift(data []byte) (*driftData, bool, string, error) {
	var in driftInput
	if err := decode(data, &in); err != nil {
		return nil, false, "", err
	}
	if in.ExitCode == nil {
		return nil, false, "", errors.New("exitCode is missing")
	}
	if *in.ExitCode != 0 && *in.ExitCode != 2 {
		return nil, false, "", fmt.Errorf("exitCode %d: only a successful plan (0 or 2) is evidence", *in.ExitCode)
	}

	d := &driftData{Root: "infra/envs/lab", ExitCode: *in.ExitCode}
	changes := effective(in.Changes)
	allCreates, clusterCreated := len(changes) > 0, false
	for _, c := range changes {
		switch kind := actionKind(c.Actions); kind {
		case "create":
			d.Counts.Create++
			clusterCreated = clusterCreated || c.Address == clusterAddress
		case "update":
			d.Counts.Update++
		case "replace":
			d.Counts.Replace++
		case "delete":
			d.Counts.Delete++
		default:
			d.Counts.Other++
		}
		allCreates = allCreates && actionKind(c.Actions) == "create"
	}
	sortChanges(changes)
	d.Changes, d.ChangesTruncated = capList(changes)

	outside := []outsideChange{}
	for _, e := range in.Drift {
		if k := actionKind(e.Actions); k == "no-op" || k == "read" {
			continue
		}
		o := judgeDrift(e)
		switch o.Verdict {
		case "relevant":
			d.OutsideCounts.Relevant++
		case "expected":
			d.OutsideCounts.Expected++
		default:
			d.OutsideCounts.Irrelevant++
		}
		outside = append(outside, o)
	}
	slices.SortFunc(outside, func(a, b outsideChange) int { return cmp.Compare(a.Address, b.Address) })
	d.ChangedOutsideTerraform, _ = capList(outside)

	n := len(changes)
	switch {
	case n == 0 && d.ExitCode == 0:
		d.Result = "no changes"
	case n == 0:
		d.Result = "outputs only"
	case allCreates && clusterCreated:
		d.Result = "lab down"
	default:
		d.Result = "changes"
	}

	headline := map[string]string{
		"no changes":   "no drift: the lab matches main",
		"outputs only": "no resource changes; only output values differ",
		"lab down":     fmt.Sprintf("the lab is down: the plan would create all %s, including the cluster", plural(n, "resource", "resources")),
		"changes": fmt.Sprintf("%s to review: %d create, %d update, %d replace, %d delete",
			plural(n, "change", "changes"), d.Counts.Create, d.Counts.Update, d.Counts.Replace, d.Counts.Delete),
	}[d.Result]
	if d.Result == "changes" && d.Counts.Other > 0 {
		headline += fmt.Sprintf(", %d other", d.Counts.Other)
	}
	if len(outside) > 0 {
		oc := d.OutsideCounts
		headline += fmt.Sprintf("; %s outside Terraform: %d relevant, %d expected (GKE upgrades, spend guard), %d not used by this plan",
			plural(len(outside), "object changed", "objects changed"), oc.Relevant, oc.Expected, oc.Irrelevant)
	}
	attention := d.Result == "changes" || d.OutsideCounts.Relevant > 0
	return d, attention, headline, nil
}

// judgeDrift decides whether a change made outside Terraform needs a look.
func judgeDrift(e driftEntry) outsideChange {
	o := outsideChange{Address: e.Address, Actions: e.Actions, Attributes: []string{}, RelevantAttributes: []string{}}
	if o.Actions == nil {
		o.Actions = []string{}
	}
	if e.Attributes != nil {
		o.Attributes = *e.Attributes
	}
	if e.RelevantAttributes != nil {
		o.RelevantAttributes = *e.RelevantAttributes
	}

	allowed := expectedDrift[resourceType(e.Address)]
	unexpected := func(attrs []string) bool {
		return slices.ContainsFunc(attrs, func(a string) bool { return !slices.Contains(allowed, a) })
	}
	switch {
	// Deleted outside Terraform, or an input that doesn't say which attributes changed.
	case actionKind(e.Actions) == "delete", e.RelevantAttributes == nil:
		o.Verdict = "relevant"
	case unexpected(o.RelevantAttributes):
		o.Verdict = "relevant"
	case len(o.Attributes) > 0 && !unexpected(o.Attributes):
		o.Verdict = "expected"
	case len(o.RelevantAttributes) > 0:
		o.Verdict = "expected" // relevant, but only attributes GKE or the spend guard change
	default:
		o.Verdict = "irrelevant"
	}
	return o
}

// resourceType returns google_container_cluster for google_container_cluster.mamori, also inside
// a module (module.x.google_container_cluster.mamori) or with an index (...mamori["a"]).
func resourceType(address string) string {
	parts := strings.Split(address, ".")
	for len(parts) >= 2 && parts[0] == "module" {
		parts = parts[2:]
	}
	if len(parts) > 0 && parts[0] == "data" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// effective drops no-op and read entries, which don't change anything, and never returns nil.
func effective(in []resourceChange) []resourceChange {
	out := []resourceChange{}
	for _, c := range in {
		if k := actionKind(c.Actions); k == "no-op" || k == "read" {
			continue
		}
		if c.Actions == nil {
			c.Actions = []string{}
		}
		out = append(out, c)
	}
	return out
}

// actionKind names Terraform's action lists: ["create"], ["update"], ["delete"], ["no-op"],
// ["read"], and ["delete","create"] or ["create","delete"] for a replacement.
func actionKind(actions []string) string {
	if len(actions) == 2 && slices.Contains(actions, "create") && slices.Contains(actions, "delete") {
		return "replace"
	}
	return strings.Join(actions, ",")
}

func sortChanges(c []resourceChange) {
	slices.SortFunc(c, func(a, b resourceChange) int { return cmp.Compare(a.Address, b.Address) })
}
