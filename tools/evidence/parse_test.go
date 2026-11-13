package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseCIRuns(t *testing.T) {
	t.Run("REST API page and failing workflow", func(t *testing.T) {
		data := `{"total_count": 2, "workflow_runs": [
			{"name": "ci", "path": ".github/workflows/ci.yml", "event": "push", "status": "completed", "conclusion": "timed_out", "head_branch": "main", "run_number": 5, "updated_at": "2026-11-15T10:00:00Z"},
			{"name": "ci", "path": ".github/workflows/ci.yml", "event": "push", "status": "completed", "conclusion": "success", "head_branch": "main", "run_number": 5, "updated_at": "2026-11-15T09:00:00Z"},
			{"name": "ci", "path": ".github/workflows/ci.yml", "event": "push", "status": "completed", "conclusion": "success", "head_branch": "feature", "run_number": 6}
		]}`
		d, attention, headline, err := parseCIRuns([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if !attention || d.Failing != 1 || len(d.Workflows) != 1 || d.Workflows[0].Conclusion != "timed_out" {
			t.Errorf("got %+v attention=%v", d, attention)
		}
		if !strings.Contains(headline, "1 not passing (ci: timed_out)") {
			t.Errorf("headline = %q", headline)
		}
	})
	t.Run("no completed runs", func(t *testing.T) {
		d, attention, headline, err := parseCIRuns([]byte(`[{"name": "ci", "event": "push", "status": "queued", "head_branch": "main"}]`))
		if err != nil || attention || len(d.Workflows) != 0 || headline != "no completed runs on main yet" {
			t.Errorf("got %+v %v %q %v", d, attention, headline, err)
		}
	})
	t.Run("object without workflow_runs", func(t *testing.T) {
		if _, _, _, err := parseCIRuns([]byte(`{"message": "Not Found"}`)); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("not a list", func(t *testing.T) {
		if _, _, _, err := parseCIRuns([]byte(`"text"`)); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("invalid page", func(t *testing.T) {
		if _, _, _, err := parseCIRuns([]byte(`{"workflow_runs": 3}`)); err == nil {
			t.Error("want an error")
		}
	})
}

func TestParseTrivy(t *testing.T) {
	t.Run("wrong schema version", func(t *testing.T) {
		if _, _, _, err := parseTrivy([]byte(`{"SchemaVersion": 1}`)); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("clean", func(t *testing.T) {
		d, attention, headline, err := parseTrivy([]byte(`{"SchemaVersion": 2, "Results": []}`))
		if err != nil || attention || d.CIGateFindings != 0 || d.Findings == nil {
			t.Errorf("got %+v %v %v", d, attention, err)
		}
		if headline != "0 vulnerabilities, 0 misconfigurations, 0 secrets; 0 findings would fail CI's scan" {
			t.Errorf("headline = %q", headline)
		}
	})
	t.Run("secret and truncation", func(t *testing.T) {
		var vulns []string
		for i := range maxListItems + 5 {
			vulns = append(vulns, fmt.Sprintf(`{"VulnerabilityID": "T-%03d", "Severity": "CRITICAL"}`, i))
		}
		data := fmt.Sprintf(`{"SchemaVersion": 2, "Results": [
			{"Target": "a", "Vulnerabilities": [%s]},
			{"Target": ".env.example", "Secrets": [{"RuleID": "github-pat", "Severity": "CRITICAL", "Title": "GitHub token", "StartLine": 3}, {"RuleID": "x", "Severity": "LOW"}]}
		]}`, strings.Join(vulns, ","))
		d, attention, _, err := parseTrivy([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		// Unfixed vulnerabilities don't fail CI (--ignore-unfixed); a HIGH or CRITICAL secret does.
		if !attention || d.CIGateFindings != 1 || d.Secrets.total() != 2 || d.Vulnerabilities.Critical != maxListItems+5 {
			t.Errorf("got gate=%d secrets=%+v vulns=%+v", d.CIGateFindings, d.Secrets, d.Vulnerabilities)
		}
		if len(d.Findings) != maxListItems || !d.FindingsTruncated {
			t.Errorf("findings = %d truncated=%v", len(d.Findings), d.FindingsTruncated)
		}
	})
	t.Run("severity names", func(t *testing.T) {
		var c severityCounts
		for _, s := range []string{"critical", "HIGH", "Medium", "LOW", "", "weird"} {
			c.add(s)
		}
		if c.String() != "1 critical, 1 high, 1 medium, 1 low, 2 unknown" {
			t.Errorf("counts = %s", c)
		}
		if severityRank("medium") != 2 || severityRank("LOW") != 3 || severityRank("?") != 4 {
			t.Error("severityRank")
		}
	})
}

func TestParseGitleaks(t *testing.T) {
	var items []string
	for i := range maxListItems + 1 {
		items = append(items, fmt.Sprintf(`{"RuleID": "generic-api-key", "File": "f%03d", "StartLine": 1, "Commit": "0123456789abcdef0123", "Secret": "REDACTED", "Email": "a@b.example"}`, i))
	}
	d, attention, headline, err := parseGitleaks([]byte("[" + strings.Join(items, ",") + "]"))
	if err != nil {
		t.Fatal(err)
	}
	if !attention || d.Findings != maxListItems+1 || !d.ItemsTruncated || d.ByRule["generic-api-key"] != maxListItems+1 {
		t.Errorf("got %+v", d)
	}
	if d.Items[0].Commit != "0123456789ab" {
		t.Errorf("commit = %q, want 12 characters", d.Items[0].Commit)
	}
	if !strings.HasPrefix(headline, "51 possible secrets") {
		t.Errorf("headline = %q", headline)
	}
	if _, _, _, err := parseGitleaks([]byte(`{}`)); err == nil {
		t.Error("an object is not a gitleaks report")
	}
}

func TestParseSBOM(t *testing.T) {
	d, attention, headline, err := parseSBOM([]byte(`{"spdxVersion": "SPDX-2.3", "packages": [
		{"SPDXID": "SPDXRef-DocumentRoot-Directory-x"},
		{"SPDXID": "a", "externalRefs": [{"referenceType": "cpe23Type", "referenceLocator": "cpe:x"}, {"referenceType": "purl", "referenceLocator": "pkg:golang/x@1"}]},
		{"SPDXID": "b", "externalRefs": [{"referenceType": "purl", "referenceLocator": "not-a-purl"}]},
		{"SPDXID": "c", "externalRefs": [{"referenceType": "purl", "referenceLocator": "pkg:nopath"}]}
	]}`))
	if err != nil || attention || d.Packages != 3 || d.ByType["golang"] != 1 || d.ByType["other"] != 2 {
		t.Errorf("got %+v %v %v", d, attention, err)
	}
	if headline != "SPDX-2.3, 3 packages" || d.Creators == nil {
		t.Errorf("headline = %q creators=%v", headline, d.Creators)
	}

	d, attention, _, err = parseSBOM([]byte(`{"spdxVersion": "SPDX-2.2", "packages": []}`))
	if err != nil || !attention || d.Packages != 0 {
		t.Errorf("an empty SBOM needs a look: %+v %v %v", d, attention, err)
	}
	if _, _, _, err := parseSBOM([]byte(`{"bomFormat": "CycloneDX"}`)); err == nil {
		t.Error("CycloneDX is not accepted")
	}
}

func TestParseDrift(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		result    string
		attention bool
		headline  string
	}{
		{"no changes", `{"exitCode": 0, "changes": [{"address": "a", "actions": ["no-op"]}], "drift": []}`,
			"no changes", false, "no drift: the lab matches main"},
		{"outputs only", `{"exitCode": 2, "changes": []}`, "outputs only", false, "no resource changes; only output values differ"},
		{"lab down", `{"exitCode": 2, "changes": [{"address": "google_container_cluster.mamori", "actions": ["create"]}, {"address": "b", "actions": ["create"]}]}`,
			"lab down", false, "the lab is down: the plan would create all 2 resources, including the cluster"},
		{"one missing resource is drift, not a down lab", `{"exitCode": 2, "changes": [{"address": "google_billing_budget.lab", "actions": ["create"]}]}`,
			"changes", true, "1 change to review: 1 create, 0 update, 0 replace, 0 delete"},
		{"mixed", `{"exitCode": 2, "changes": [
			{"address": "d", "actions": ["delete"]}, {"address": "r", "actions": ["create", "delete"]},
			{"address": "r2", "actions": ["delete", "create"]}, {"address": "f", "actions": ["forget"]}, {"address": "n", "actions": null}]}`,
			"changes", true, "5 changes to review: 0 create, 0 update, 2 replace, 1 delete, 2 other"},
		{"changed outside Terraform only", `{"exitCode": 0, "drift": [{"address": "x", "actions": ["update"]}]}`,
			"no changes", true, "no drift: the lab matches main; 1 object changed outside Terraform"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, attention, headline, err := parseDrift([]byte(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if d.Result != tc.result || attention != tc.attention || headline != tc.headline {
				t.Errorf("got result=%q attention=%v headline=%q", d.Result, attention, headline)
			}
		})
	}
	for _, bad := range []string{`{}`, `{"exitCode": 1}`, `[]`} {
		if _, _, _, err := parseDrift([]byte(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	if d, _, _, _ := parseDrift([]byte(`{"exitCode": 2, "changes": [{"address": "n", "actions": null}]}`)); d.Changes[0].Actions == nil {
		t.Error("actions must encode as [] rather than null")
	}
}

func TestClusterSection(t *testing.T) {
	cluster := input{ref: inputRef{File: inputGKECluster, Status: statusCollected}, data: []byte(`{"name": "mamori", "status": "RECONCILING",
		"currentMasterVersion": "1.37.3", "nodePools": [{"name": "spot-pool", "version": "1.37.2", "status": "RUNNING_WITH_ERROR"}]}`)}
	nodes := func(data string) input {
		return input{ref: inputRef{File: inputNodes, Status: statusCollected}, data: []byte(data)}
	}

	t.Run("spend guard scaled the pool to zero", func(t *testing.T) {
		s := clusterSection(cluster, nodes(`{"items": []}`))
		if !s.Attention || !strings.Contains(s.Headline, "no nodes") || !strings.Contains(s.Headline, "cluster status RECONCILING") ||
			!strings.Contains(s.Headline, "node pool spot-pool RUNNING_WITH_ERROR") {
			t.Errorf("headline = %q", s.Headline)
		}
	})
	t.Run("node not ready", func(t *testing.T) {
		s := clusterSection(cluster, nodes(`{"items": [{"metadata": {"name": "n1"}, "status": {"conditions": [{"type": "Ready", "status": "False"}]}}]}`))
		if !strings.Contains(s.Headline, "0/1 nodes Ready") || !strings.Contains(s.Headline, "1 node not Ready") {
			t.Errorf("headline = %q", s.Headline)
		}
	})
	t.Run("nodes invalid", func(t *testing.T) {
		s := clusterSection(cluster, nodes(`[`))
		if s.Status != statusCollected || s.Data.NodesStatus != statusNotCollected || !strings.Contains(s.Data.NodesReason, "nodes.json is not valid") {
			t.Errorf("got %q %+v", s.Status, s.Data)
		}
	})
	t.Run("nodes missing", func(t *testing.T) {
		missing := input{ref: inputRef{File: inputNodes, Status: statusNotCollected, Reason: "endpoint unreachable"}}
		s := clusterSection(cluster, missing)
		if s.Data.NodesStatus != statusNotCollected || !strings.Contains(s.Headline, "nodes not collected: endpoint unreachable") {
			t.Errorf("headline = %q", s.Headline)
		}
		if len(s.Inputs) != 2 || s.Inputs[1].Reason != "endpoint unreachable" {
			t.Errorf("inputs = %+v", s.Inputs)
		}
	})
	t.Run("cluster without version", func(t *testing.T) {
		s := clusterSection(input{ref: inputRef{File: inputGKECluster, Status: statusCollected}, data: []byte(`{"name": "mamori"}`)}, nodes(`{}`))
		if s.Status != statusNotCollected || len(s.Inputs) != 2 {
			t.Errorf("got %q %v", s.Status, s.Inputs)
		}
	})
}

func TestParseConstraints(t *testing.T) {
	d, attention, headline, err := parseConstraints([]byte(`{"items": [
		{"kind": "K8sA", "metadata": {"name": "a"}, "status": {"violations": [{"kind": "Pod", "name": "p"}]}},
		{"kind": "K8sB", "metadata": {"name": "b"}, "spec": {"enforcementAction": "dryrun"}, "status": {"auditTimestamp": "2026-11-16T00:00:00Z", "totalViolations": 30}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	// A constraint without totalViolations counts its stored list; deny is Gatekeeper's default.
	if !attention || d.TotalViolations != 31 || d.ViolationsByAction["deny"] != 1 || d.ViolationsByAction["dryrun"] != 30 || d.NotAudited != 1 {
		t.Errorf("got %+v", d)
	}
	want := "2 constraints, 31 violations (deny 1, dryrun 30), last audit 2026-11-16T00:00:00Z, 1 not audited yet"
	if headline != want {
		t.Errorf("headline = %q, want %q", headline, want)
	}

	d, attention, headline, err = parseConstraints([]byte(`{"items": []}`))
	if err != nil || attention || d.Constraints != 0 || headline != "no constraints installed" {
		t.Errorf("got %+v %v %q %v", d, attention, headline, err)
	}
	if _, _, _, err := parseConstraints([]byte(`[]`)); err == nil {
		t.Error("want an error")
	}
}

func TestParseArgoApps(t *testing.T) {
	d, attention, headline, err := parseArgoApps([]byte(`{"items": [{"metadata": {"name": "x", "namespace": "argocd"}, "spec": {}}]}`))
	if err != nil || !attention || d.SyncedHealthy != 0 || headline != "0/1 applications Synced and Healthy (not: x Unknown/Unknown)" {
		t.Errorf("got %+v %v %q %v", d, attention, headline, err)
	}
	d, attention, headline, err = parseArgoApps([]byte(`{"items": []}`))
	if err != nil || !attention || d.Applications != 0 || headline != "no Argo CD applications found" {
		t.Errorf("got %+v %v %q %v", d, attention, headline, err)
	}
	if _, _, _, err := parseArgoApps([]byte(`{"items": 1}`)); err == nil {
		t.Error("want an error")
	}
}

func TestParseSLOStatus(t *testing.T) {
	d, attention, headline, err := parseSLOStatus([]byte(`{"slos": [
		{"name": "projects/1/services/s/serviceLevelObjectives/spent", "service": "projects/1/services/s", "goal": 0.99, "calendarPeriod": "MONTH", "budgetFraction": -0.4},
		{"name": "projects/1/services/s/serviceLevelObjectives/odd", "service": "projects/1/services/s", "goal": 0.9, "rollingPeriod": "90000s", "budgetFraction": 0.5},
		{"name": "projects/1/services/s/serviceLevelObjectives/bad", "service": "projects/1/services/s", "goal": 0.9, "rollingPeriod": "soon"},
		{"name": "projects/1/services/s/serviceLevelObjectives/none", "service": "projects/1/services/s", "goal": 0.9}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !attention || d.Exhausted != 1 || d.NoData != 2 {
		t.Errorf("got %+v", d)
	}
	windows := map[string]string{}
	for _, s := range d.SLOs {
		windows[s.DisplayName] = s.Window
	}
	if windows["spent"] != "calendar month" || windows["odd"] != "rolling 25h0m0s" || windows["bad"] != "rolling soon" || windows["none"] != "unknown" {
		t.Errorf("windows = %v", windows)
	}
	if !strings.Contains(headline, "least budget left: spent (-40.0 %)") {
		t.Errorf("headline = %q", headline)
	}

	d, attention, headline, err = parseSLOStatus([]byte(`{"slos": []}`))
	if err != nil || attention || len(d.SLOs) != 0 || headline != "no SLOs defined in Cloud Monitoring yet" {
		t.Errorf("got %+v %v %q %v", d, attention, headline, err)
	}
	if _, _, _, err := parseSLOStatus([]byte(`{}`)); err == nil {
		t.Error("want an error when slos is missing")
	}
	if _, _, _, err := parseSLOStatus([]byte(`{"slos": 1}`)); err == nil {
		t.Error("want an error")
	}
}

func TestMarkdownEscaping(t *testing.T) {
	if got := mdCell(" a|b\r\nc\nd\re "); got != `a\|b c d e` {
		t.Errorf("mdCell = %q", got)
	}
	if got := mdText("a\nb"); got != "a b" {
		t.Errorf("mdText = %q", got)
	}
	if got := truncate(strings.Repeat("x", 10), 5); got != "xxxx…" {
		t.Errorf("truncate = %q", got)
	}
	if link("t", "") != "t" || dash("") != "-" || bytesText(0) != "-" || optPercent(nil) != "no data" {
		t.Error("helpers")
	}
}

func TestSummaryShowsDetailsForEveryCollectedSection(t *testing.T) {
	dir, err := openInputDir("testdata/inputs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	b := assemble(dir, source{RunID: "1"}, fixedNow)

	md := renderSummary(b)
	for _, want := range []string{
		"| CI on main | collected | yes |",
		"| Gatekeeper audit | collected | yes |",
		"| K8sRequiredProbes/pr-02-probes | PR-02 | dryrun | 2 |",
		`container <main> has no readinessProbe \| add one`, // a pipe in a message can't break the table
		"container <redis> has no livenessProbe",            // nor can a newline
		"| CUJ browse | browse latency | 95.0 % | rolling 28d | no data | no data | no data |",
		"workflow run 1",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("summary.md lacks %q", want)
		}
	}
}
