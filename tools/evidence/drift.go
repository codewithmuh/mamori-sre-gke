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

// driftInput is terraform-drift.json, which the workflow builds with jq from
// `terraform show -json` of a `plan -detailed-exitcode` run. Only addresses and actions are kept:
// the plan's values can hold sensitive data, so they never leave the runner.
type driftInput struct {
	ExitCode *int             `json:"exitCode"`
	Changes  []resourceChange `json:"changes"`
	Drift    []resourceChange `json:"drift"`
}

type resourceChange struct {
	Address string   `json:"address"`
	Actions []string `json:"actions"`
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
	// Objects that changed outside Terraform since the last apply (Terraform's resource_drift).
	ChangedOutsideTerraform []resourceChange `json:"changedOutsideTerraform"`
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
	outside := effective(in.Drift)
	sortChanges(outside)
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
		headline += fmt.Sprintf("; %s outside Terraform", plural(len(outside), "object changed", "objects changed"))
	}
	attention := d.Result == "changes" || len(outside) > 0
	return d, attention, headline, nil
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
