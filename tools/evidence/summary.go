package main

import (
	"fmt"
	"strconv"
	"strings"
)

// renderSummary writes summary.md: the same content as bundle.json, for people. Every value comes
// from the bundle, so the two can't disagree.
func renderSummary(b bundle) string {
	var w mdWriter
	src := b.Source

	w.line("# Evidence bundle")
	w.line()
	w.linef("Generated %s from commit `%s` (%s) of `%s` by %s.", b.GeneratedAt, shortSHA(src.Commit), dash(src.Ref),
		dash(src.Repository), runLink(src))
	w.linef("Schema `%s`. A lab with synthetic traffic: these numbers describe the lab, not a production service.", b.SchemaVersion)
	w.line()
	w.linef("**%d of %d areas collected.** Needs a look: %s.", b.Overview.Collected, b.Overview.Sections, attentionList(b))
	w.line()
	w.line("| Area | Status | Needs a look | Summary |")
	w.line("|---|---|---|---|")
	for _, si := range b.Sections.list() {
		status := si.status
		if si.status != statusCollected && si.expected {
			status += " (expected)"
		}
		w.row(si.title, status, yesNo(si.attention), si.headline)
	}

	s := b.Sections
	ciDetails(&w, s.CI)
	githubDetails(&w, s.GitHub)
	trivyDetails(&w, s.Trivy)
	gitleaksDetails(&w, s.Gitleaks)
	sbomDetails(&w, s.SBOM)
	driftDetails(&w, s.TerraformDrift)
	clusterDetails(&w, s.Cluster)
	gatekeeperDetails(&w, s.Gatekeeper)
	argoDetails(&w, s.ArgoCD)
	sloDetails(&w, s.SLOs)

	w.heading("Inputs")
	w.line("Copies of these files are in `inputs/`; `SHA256SUMS` covers every file of the bundle.")
	w.line()
	w.line("| File | Status | SHA-256 | Bytes |")
	w.line("|---|---|---|---|")
	for _, si := range b.Sections.list() {
		for _, in := range si.inputs {
			status := in.Status
			if in.Reason != "" {
				status += ": " + in.Reason
			}
			w.row("`"+in.File+"`", status, dash(in.SHA256), bytesText(in.Bytes))
		}
	}
	w.line()
	w.line("How to check this bundle: [docs/compliance/evidence-bundle.md](https://github.com/codewithmuh/mamori-sre-gke/blob/main/docs/compliance/evidence-bundle.md).")
	return w.String()
}

// sectionHeader writes the heading and, for a section that wasn't collected, the reason. It
// returns false when there are no details to show.
func sectionHeader[T any](w *mdWriter, s section[T]) bool {
	w.heading(s.Title)
	w.line(sentence(mdText(s.Headline)))
	return s.Status == statusCollected && s.Data != nil
}

func ciDetails(w *mdWriter, s section[ciData]) {
	if !sectionHeader(w, s) || len(s.Data.Workflows) == 0 {
		return
	}
	w.line()
	w.line("| Workflow | Result | Event | Commit | Finished | Run |")
	w.line("|---|---|---|---|---|---|")
	for _, r := range s.Data.Workflows {
		w.row(r.Name+" (`"+r.Path+"`)", r.Conclusion, r.Event, "`"+r.Commit+"`", r.FinishedAt, link("#"+strconv.Itoa(r.RunNumber), r.URL))
	}
}

func githubDetails(w *mdWriter, s section[githubData]) {
	if !sectionHeader(w, s) || len(s.Data.Checks) == 0 {
		return
	}
	w.line()
	w.line("| Control | Required | Status | Detail |")
	w.line("|---|---|---|---|")
	for _, c := range s.Data.Checks {
		w.row(c.Control, yesNo(c.Required), c.Status, dash(c.Detail))
	}
}

func trivyDetails(w *mdWriter, s section[trivyData]) {
	if !sectionHeader(w, s) {
		return
	}
	d := s.Data
	w.line()
	w.linef("Trivy %s, scanned %s.", dash(d.TrivyVersion), dash(d.ScannedAt))
	w.line()
	w.line("| Kind | Critical | High | Medium | Low | Unknown |")
	w.line("|---|---|---|---|---|---|")
	for _, k := range []struct {
		name string
		c    severityCounts
	}{{"Vulnerabilities", d.Vulnerabilities}, {"Misconfigurations", d.Misconfigurations}, {"Secrets", d.Secrets}} {
		w.row(k.name, itoa(k.c.Critical), itoa(k.c.High), itoa(k.c.Medium), itoa(k.c.Low), itoa(k.c.Unknown))
	}
	if len(d.Findings) == 0 {
		return
	}
	w.line()
	w.line("HIGH and CRITICAL findings" + truncatedNote(d.FindingsTruncated) + ":")
	w.line()
	w.line("| Severity | Kind | ID | Where | Package | Fixed in | Title |")
	w.line("|---|---|---|---|---|---|---|")
	for _, f := range d.Findings {
		where := f.Target
		if f.Line > 0 {
			where += ":" + itoa(f.Line)
		}
		pkg := f.Package
		if pkg != "" && f.InstalledVersion != "" {
			pkg += " " + f.InstalledVersion
		}
		w.row(f.Severity, f.Class, f.ID, where, dash(pkg), dash(f.FixedVersion), f.Title)
	}
}

func gitleaksDetails(w *mdWriter, s section[gitleaksData]) {
	if !sectionHeader(w, s) || len(s.Data.Items) == 0 {
		return
	}
	w.line()
	w.linef("By rule: %s. Secrets are redacted; rotate any real one, then remove it from history or allow-list a false positive.", countsText(s.Data.ByRule))
	w.line()
	w.line("| Rule | File | Line | Commit | Date |")
	w.line("|---|---|---|---|---|")
	for _, it := range s.Data.Items {
		w.row(it.Rule, it.File, itoa(it.Line), "`"+it.Commit+"`", it.Date)
	}
	if s.Data.ItemsTruncated {
		w.line()
		w.linef("Only the first %d of %d are listed.", maxListItems, s.Data.Findings)
	}
}

func sbomDetails(w *mdWriter, s section[sbomData]) {
	if !sectionHeader(w, s) {
		return
	}
	d := s.Data
	w.line()
	w.linef("Document `%s`, created %s by %s. Packages by type: %s.", d.DocumentName, dash(d.Created),
		dash(strings.Join(d.Creators, ", ")), dash(countsText(d.ByType)))
	w.line("The full SBOM is `inputs/" + inputSBOM + "`.")
}

func driftDetails(w *mdWriter, s section[driftData]) {
	if !sectionHeader(w, s) {
		return
	}
	d := s.Data
	w.line()
	w.linef("`terraform plan -detailed-exitcode` in `%s` exited %d (result: %s).", d.Root, d.ExitCode, d.Result)
	if len(d.Changes) > 0 && d.Result != "lab down" {
		w.line()
		w.line("| Resource | Action |")
		w.line("|---|---|")
		for _, c := range d.Changes {
			w.row("`"+c.Address+"`", actionKind(c.Actions))
		}
		if d.ChangesTruncated {
			w.line()
			w.linef("Only the first %d changes are listed.", maxListItems)
		}
	}
	if len(d.ChangedOutsideTerraform) > 0 {
		w.line()
		w.line("Changed outside Terraform since the last apply (attribute names only; values never leave the runner):")
		w.line()
		w.line("| Resource | Change | Attributes | Used by this plan | Verdict |")
		w.line("|---|---|---|---|---|")
		for _, c := range d.ChangedOutsideTerraform {
			w.row("`"+c.Address+"`", actionKind(c.Actions), dash(strings.Join(c.Attributes, ", ")),
				dash(strings.Join(c.RelevantAttributes, ", ")), c.Verdict)
		}
	}
}

func clusterDetails(w *mdWriter, s section[clusterData]) {
	if !sectionHeader(w, s) {
		return
	}
	d := s.Data
	w.line()
	w.linef("Cluster `%s` in %s.", d.Name, dash(d.Location))
	w.line()
	w.line("| Node pool | Version | Status | Machine type | Spot | Autoscaling |")
	w.line("|---|---|---|---|---|---|")
	for _, p := range d.NodePools {
		w.row(p.Name, p.Version, p.Status, p.MachineType, yesNo(p.Spot), yesNo(p.Autoscaling))
	}
	if d.NodesStatus != statusCollected || len(d.Nodes) == 0 {
		return
	}
	w.line()
	w.line("| Node | Pool | Kubelet | Ready | Spot | Created | OS image |")
	w.line("|---|---|---|---|---|---|---|")
	for _, n := range d.Nodes {
		w.row(n.Name, n.Pool, n.KubeletVersion, yesNo(n.Ready), yesNo(n.Spot), n.CreatedAt, n.OSImage)
	}
}

func gatekeeperDetails(w *mdWriter, s section[gatekeeperData]) {
	if !sectionHeader(w, s) || len(s.Data.Items) == 0 {
		return
	}
	w.line()
	w.line("| Constraint | Control | Action | Violations | Last audit |")
	w.line("|---|---|---|---|---|")
	for _, c := range s.Data.Items {
		w.row(c.Kind+"/"+c.Name, dash(c.ControlID), c.EnforcementAction, itoa(c.TotalViolations), dash(c.AuditTimestamp))
	}
	started := false
	for _, c := range s.Data.Items {
		for _, v := range c.Violations {
			if !started {
				w.line()
				w.line("Violations stored by the audit (Gatekeeper keeps a limited number per constraint; the counts above cover all):")
				w.line()
				w.line("| Constraint | Object | Message |")
				w.line("|---|---|---|")
				started = true
			}
			obj := v.Kind + " " + v.Name
			if v.Namespace != "" {
				obj = v.Kind + " " + v.Namespace + "/" + v.Name
			}
			w.row(c.Kind+"/"+c.Name, obj, v.Message)
		}
	}
}

func argoDetails(w *mdWriter, s section[argoCDData]) {
	if !sectionHeader(w, s) || len(s.Data.Items) == 0 {
		return
	}
	w.line()
	w.line("| Application | Sync | Health | Target | Synced revision | Last operation |")
	w.line("|---|---|---|---|---|---|")
	for _, a := range s.Data.Items {
		w.row(a.Namespace+"/"+a.Name, a.SyncStatus, a.HealthStatus, dash(a.TargetRevision), dash(a.SyncedRevision), dash(a.LastOperation))
	}
}

func sloDetails(w *mdWriter, s section[sloData]) {
	if !sectionHeader(w, s) || len(s.Data.SLOs) == 0 {
		return
	}
	w.line()
	w.line("| Service | SLO | Goal | Window | Compliance | Budget left | State |")
	w.line("|---|---|---|---|---|---|---|")
	for _, o := range s.Data.SLOs {
		w.row(o.Service, o.DisplayName, percent(o.Goal), o.Window, optPercent(o.Compliance), optPercent(o.BudgetRemaining), o.State)
	}
}

// mdWriter builds Markdown line by line.
type mdWriter struct{ strings.Builder }

// line writes the parts and a newline. strings.Builder never returns an error.
func (w *mdWriter) line(parts ...string) {
	for _, p := range parts {
		_, _ = w.WriteString(p)
	}
	_ = w.WriteByte('\n')
}

func (w *mdWriter) linef(format string, args ...any) { w.line(fmt.Sprintf(format, args...)) }

func (w *mdWriter) heading(title string) {
	w.line()
	w.line("## " + title)
	w.line()
}

// row writes one table row. Cells are escaped, because some come from cluster objects (violation
// messages, names) and could otherwise break the table.
func (w *mdWriter) row(cells ...string) {
	escaped := make([]string, len(cells))
	for i, c := range cells {
		escaped[i] = mdCell(c)
	}
	w.line("| " + strings.Join(escaped, " | ") + " |")
}

func mdCell(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "|", `\|`).Replace(s)
	return strings.TrimSpace(s)
}

// sentence turns a headline into a sentence: capital first letter, full stop.
func sentence(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:]) + "."
}

// mdText keeps headlines on one line.
func mdText(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

func link(text, url string) string {
	if url == "" {
		return text
	}
	return "[" + text + "](" + url + ")"
}

func runLink(src source) string {
	text := "workflow run " + dash(src.RunID)
	if src.Workflow != "" {
		text = src.Workflow + " run " + dash(src.RunID)
	}
	return link(text, src.RunURL)
}

func attentionList(b bundle) string {
	if len(b.Overview.Attention) == 0 {
		return "nothing"
	}
	titles := []string{}
	for _, si := range b.Sections.list() {
		switch {
		case si.attention && si.status != statusCollected:
			titles = append(titles, si.title+" (expected, not collected)")
		case si.attention:
			titles = append(titles, si.title)
		}
	}
	return strings.Join(titles, ", ")
}

func truncatedNote(truncated bool) string {
	if truncated {
		return fmt.Sprintf(" (first %d)", maxListItems)
	}
	return ""
}

func optPercent(f *float64) string {
	if f == nil {
		return "no data"
	}
	return percent(*f)
}

func bytesText(n int64) string {
	if n == 0 {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }
