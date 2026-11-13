package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// severityCounts counts findings per Trivy severity.
type severityCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
}

func (c *severityCounts) add(severity string) {
	switch strings.ToUpper(severity) {
	case "CRITICAL":
		c.Critical++
	case "HIGH":
		c.High++
	case "MEDIUM":
		c.Medium++
	case "LOW":
		c.Low++
	default:
		c.Unknown++
	}
}

func (c severityCounts) total() int { return c.Critical + c.High + c.Medium + c.Low + c.Unknown }

func (c severityCounts) String() string {
	return fmt.Sprintf("%d critical, %d high, %d medium, %d low, %d unknown", c.Critical, c.High, c.Medium, c.Low, c.Unknown)
}

func highOrCritical(severity string) bool {
	s := strings.ToUpper(severity)
	return s == "HIGH" || s == "CRITICAL"
}

// trivyReport is the part of Trivy's JSON report (schema version 2) that the bundle uses.
type trivyReport struct {
	SchemaVersion int    `json:"SchemaVersion"`
	CreatedAt     string `json:"CreatedAt"`
	Trivy         struct {
		Version string `json:"Version"`
	} `json:"Trivy"`
	Results []struct {
		Target          string `json:"Target"`
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			PkgName          string `json:"PkgName"`
			InstalledVersion string `json:"InstalledVersion"`
			FixedVersion     string `json:"FixedVersion"`
			Severity         string `json:"Severity"`
			Title            string `json:"Title"`
		} `json:"Vulnerabilities"`
		Misconfigurations []struct {
			ID       string `json:"ID"`
			Title    string `json:"Title"`
			Severity string `json:"Severity"`
			Status   string `json:"Status"`
		} `json:"Misconfigurations"`
		Secrets []struct {
			RuleID    string `json:"RuleID"`
			Title     string `json:"Title"`
			Severity  string `json:"Severity"`
			StartLine int    `json:"StartLine"`
		} `json:"Secrets"`
	} `json:"Results"`
}

type trivyData struct {
	TrivyVersion      string         `json:"trivyVersion"`
	ScannedAt         string         `json:"scannedAt"`
	Vulnerabilities   severityCounts `json:"vulnerabilities"`
	Misconfigurations severityCounts `json:"misconfigurations"`
	Secrets           severityCounts `json:"secrets"`
	// Findings that would fail CI's own Trivy step: HIGH or CRITICAL, and for vulnerabilities only
	// those with a fixed version (CI runs with --ignore-unfixed).
	CIGateFindings int `json:"ciGateFindings"`
	// HIGH and CRITICAL findings, at most maxListItems.
	Findings          []trivyFinding `json:"findings"`
	FindingsTruncated bool           `json:"findingsTruncated"`
}

type trivyFinding struct {
	Class            string `json:"class"` // vulnerability, misconfiguration or secret
	ID               string `json:"id"`
	Severity         string `json:"severity"`
	Target           string `json:"target"`
	Package          string `json:"package,omitempty"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	FixedVersion     string `json:"fixedVersion,omitempty"`
	Line             int    `json:"line,omitempty"`
	Title            string `json:"title"`
}

func parseTrivy(data []byte) (*trivyData, bool, string, error) {
	var r trivyReport
	if err := decode(data, &r); err != nil {
		return nil, false, "", err
	}
	if r.SchemaVersion != 2 {
		return nil, false, "", fmt.Errorf("SchemaVersion %d, want 2", r.SchemaVersion)
	}

	d := &trivyData{TrivyVersion: r.Trivy.Version, ScannedAt: r.CreatedAt}
	var findings []trivyFinding
	for _, res := range r.Results {
		for _, v := range res.Vulnerabilities {
			d.Vulnerabilities.add(v.Severity)
			if !highOrCritical(v.Severity) {
				continue
			}
			if v.FixedVersion != "" {
				d.CIGateFindings++
			}
			findings = append(findings, trivyFinding{
				Class: "vulnerability", ID: v.VulnerabilityID, Severity: v.Severity, Target: res.Target,
				Package: v.PkgName, InstalledVersion: v.InstalledVersion, FixedVersion: v.FixedVersion, Title: v.Title,
			})
		}
		for _, m := range res.Misconfigurations {
			// Passed checks are counted only in Trivy's own summary; keep failures.
			if m.Status != "" && m.Status != "FAIL" {
				continue
			}
			d.Misconfigurations.add(m.Severity)
			if highOrCritical(m.Severity) {
				d.CIGateFindings++
				findings = append(findings, trivyFinding{Class: "misconfiguration", ID: m.ID, Severity: m.Severity, Target: res.Target, Title: m.Title})
			}
		}
		for _, s := range res.Secrets {
			d.Secrets.add(s.Severity)
			if highOrCritical(s.Severity) {
				d.CIGateFindings++
				findings = append(findings, trivyFinding{Class: "secret", ID: s.RuleID, Severity: s.Severity, Target: res.Target, Line: s.StartLine, Title: s.Title})
			}
		}
	}

	slices.SortFunc(findings, func(a, b trivyFinding) int {
		return cmp.Or(
			cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)),
			cmp.Compare(a.Class, b.Class),
			cmp.Compare(a.Target, b.Target),
			cmp.Compare(a.ID, b.ID),
			cmp.Compare(a.Package, b.Package),
		)
	})
	d.Findings, d.FindingsTruncated = capList(findings)

	headline := fmt.Sprintf("%s, %s, %s; %s",
		plural(d.Vulnerabilities.total(), "vulnerability", "vulnerabilities"),
		plural(d.Misconfigurations.total(), "misconfiguration", "misconfigurations"),
		plural(d.Secrets.total(), "secret", "secrets"),
		plural(d.CIGateFindings, "finding would fail CI's scan", "findings would fail CI's scan"))
	return d, d.CIGateFindings > 0, headline, nil
}

func severityRank(s string) int {
	switch strings.ToUpper(s) {
	case "CRITICAL":
		return 0
	case "HIGH":
		return 1
	case "MEDIUM":
		return 2
	case "LOW":
		return 3
	default:
		return 4
	}
}

// gitleaksFinding is one entry of gitleaks' JSON report. The workflow runs gitleaks with --redact
// and keeps only these fields: no secret, match, author or email reaches the bundle.
type gitleaksFinding struct {
	RuleID    string `json:"RuleID"`
	File      string `json:"File"`
	StartLine int    `json:"StartLine"`
	Commit    string `json:"Commit"`
	Date      string `json:"Date"`
}

type gitleaksData struct {
	Findings       int            `json:"findings"`
	ByRule         map[string]int `json:"byRule"`
	Items          []gitleaksItem `json:"items"`
	ItemsTruncated bool           `json:"itemsTruncated"`
}

type gitleaksItem struct {
	Rule   string `json:"rule"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Commit string `json:"commit"`
	Date   string `json:"date"`
}

func parseGitleaks(data []byte) (*gitleaksData, bool, string, error) {
	var report []gitleaksFinding
	if err := decode(data, &report); err != nil {
		return nil, false, "", err
	}
	d := &gitleaksData{Findings: len(report), ByRule: map[string]int{}}
	items := make([]gitleaksItem, 0, len(report))
	for _, f := range report {
		d.ByRule[f.RuleID]++
		items = append(items, gitleaksItem{Rule: f.RuleID, File: f.File, Line: f.StartLine, Commit: shortSHA(f.Commit), Date: f.Date})
	}
	slices.SortFunc(items, func(a, b gitleaksItem) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Rule, b.Rule), cmp.Compare(a.Commit, b.Commit))
	})
	d.Items, d.ItemsTruncated = capList(items)

	if d.Findings == 0 {
		return d, false, "no secrets found in the Git history", nil
	}
	return d, true, fmt.Sprintf("%s in the Git history (redacted; see items)", plural(d.Findings, "possible secret", "possible secrets")), nil
}

// capList keeps at most maxListItems entries and says whether it cut any. It never returns nil,
// so the JSON shows [] rather than null.
func capList[T any](items []T) ([]T, bool) {
	if items == nil {
		return []T{}, false
	}
	if len(items) > maxListItems {
		return items[:maxListItems], true
	}
	return items, false
}
