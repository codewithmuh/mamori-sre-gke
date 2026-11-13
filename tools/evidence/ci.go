package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Only runs that main itself started count as "CI on main". A pull request from a fork's own
// branch called main also reports head_branch "main", so pull_request runs are left out.
var mainEvents = []string{"push", "schedule", "workflow_dispatch"}

// Conclusions that don't mean something is broken.
var okConclusions = []string{"success", "skipped", "neutral"}

// githubRun holds the fields of a GitHub Actions workflow run (REST API) that the workflow keeps.
type githubRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
	RunNumber  int    `json:"run_number"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	HTMLURL    string `json:"html_url"`
}

type ciData struct {
	Branch string `json:"branch"`
	// The newest completed run of each workflow file, sorted by file.
	Workflows []workflowResult `json:"workflows"`
	Failing   int              `json:"failing"`
}

type workflowResult struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Event      string `json:"event"`
	Conclusion string `json:"conclusion"`
	Commit     string `json:"commit"`
	RunNumber  int    `json:"runNumber"`
	FinishedAt string `json:"finishedAt"`
	URL        string `json:"url"`
}

// parseCIRuns reads ci-runs.json: a JSON array of workflow runs, or the REST API's
// {"workflow_runs": [...]} object.
func parseCIRuns(data []byte) (*ciData, bool, string, error) {
	runs, err := decodeRuns(data)
	if err != nil {
		return nil, false, "", err
	}

	latest := map[string]githubRun{}
	for _, r := range runs {
		if r.Status != "completed" || r.HeadBranch != "main" || !slices.Contains(mainEvents, r.Event) {
			continue
		}
		key := cmp.Or(r.Path, r.Name)
		if cur, seen := latest[key]; !seen || newerRun(r, cur) {
			latest[key] = r
		}
	}

	d := &ciData{Branch: "main", Workflows: []workflowResult{}}
	var failing []string
	for _, r := range latest {
		d.Workflows = append(d.Workflows, workflowResult{
			Name:       r.Name,
			Path:       r.Path,
			Event:      r.Event,
			Conclusion: r.Conclusion,
			Commit:     shortSHA(r.HeadSHA),
			RunNumber:  r.RunNumber,
			FinishedAt: r.UpdatedAt,
			URL:        r.HTMLURL,
		})
	}
	slices.SortFunc(d.Workflows, func(a, b workflowResult) int { return cmp.Compare(a.Path+a.Name, b.Path+b.Name) })
	for _, w := range d.Workflows {
		if !slices.Contains(okConclusions, w.Conclusion) {
			failing = append(failing, fmt.Sprintf("%s: %s", cmp.Or(w.Name, w.Path), cmp.Or(w.Conclusion, "no conclusion")))
		}
	}
	d.Failing = len(failing)

	if len(d.Workflows) == 0 {
		return d, false, "no completed runs on main yet", nil
	}
	headline := fmt.Sprintf("%s on main, latest runs all passed", plural(len(d.Workflows), "workflow", "workflows"))
	if len(failing) > 0 {
		headline = fmt.Sprintf("%s on main, %d not passing (%s)", plural(len(d.Workflows), "workflow", "workflows"),
			len(failing), strings.Join(failing, "; "))
	}
	return d, len(failing) > 0, headline, nil
}

func decodeRuns(data []byte) ([]githubRun, error) {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") {
		var page struct {
			WorkflowRuns *[]githubRun `json:"workflow_runs"`
		}
		if err := decode(data, &page); err != nil {
			return nil, err
		}
		if page.WorkflowRuns == nil {
			return nil, fmt.Errorf("object has no workflow_runs")
		}
		return *page.WorkflowRuns, nil
	}
	var runs []githubRun
	if err := decode(data, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

// newerRun orders by run number (per workflow, it only grows), then by update time.
func newerRun(a, b githubRun) bool {
	if a.RunNumber != b.RunNumber {
		return a.RunNumber > b.RunNumber
	}
	return a.UpdatedAt > b.UpdatedAt
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
