package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Controls that live only in GitHub's settings, not in Git: the protected environment that guards
// tf-apply, the rules on main and on release tags, and what workflows may do by default. The
// workflow reads them through the REST API (read-only) into github-settings.json.

const (
	checkOK           = "ok"
	checkMissing      = "missing"
	checkNotCollected = "not collected"

	// The one required check on main (ci.yml's "CI result" job).
	requiredCheckName = "CI result"
	// The environment that alone may impersonate tf-apply.
	applyEnvironment = "lab"
)

// githubInput is github-settings.json: each API response with its HTTP status. Status 0 means the
// call wasn't made, with the reason (for example, no token with Administration read).
type githubInput struct {
	// Whether the workflow had a token with Administration read for the admin-only endpoints.
	AdminToken bool                      `json:"adminToken"`
	Responses  map[string]githubResponse `json:"responses"`
}

type githubResponse struct {
	Status int             `json:"status"`
	Reason string          `json:"reason"`
	Body   json.RawMessage `json:"body"`
}

type githubData struct {
	Checks       []githubCheck `json:"checks"`
	Missing      int           `json:"missing"`
	NotCollected int           `json:"notCollected"`
}

type githubCheck struct {
	ID      string `json:"id"`
	Control string `json:"control"`
	// A required control that is missing needs a look; a recommended one is only reported.
	Required bool   `json:"required"`
	Status   string `json:"status"` // ok, missing or not collected
	Detail   string `json:"detail"`
}

// API shapes, only the fields used.
type (
	ghEnvironment struct {
		ProtectionRules []struct {
			Type      string            `json:"type"`
			Reviewers []json.RawMessage `json:"reviewers"`
		} `json:"protection_rules"`
		DeploymentBranchPolicy *struct {
			ProtectedBranches    bool `json:"protected_branches"`
			CustomBranchPolicies bool `json:"custom_branch_policies"`
		} `json:"deployment_branch_policy"`
		CanAdminsBypass *bool `json:"can_admins_bypass"`
	}
	ghBranchPolicies struct {
		BranchPolicies []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"branch_policies"`
	}
	ghBranch struct {
		Protected  bool `json:"protected"`
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	ghRule struct {
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}
	ghRuleset struct {
		Name        string   `json:"name"`
		Target      string   `json:"target"`
		Enforcement string   `json:"enforcement"`
		Rules       []ghRule `json:"rules"`
	}
	ghProtection struct {
		RequiredPullRequestReviews *json.RawMessage `json:"required_pull_request_reviews"`
		RequiredStatusChecks       *struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
		AllowForcePushes struct {
			Enabled bool `json:"enabled"`
		} `json:"allow_force_pushes"`
		AllowDeletions struct {
			Enabled bool `json:"enabled"`
		} `json:"allow_deletions"`
	}
	ghWorkflowPermissions struct {
		DefaultWorkflowPermissions   string `json:"default_workflow_permissions"`
		CanApprovePullRequestReviews bool   `json:"can_approve_pull_request_reviews"`
	}
	ghActionsPermissions struct {
		AllowedActions     string `json:"allowed_actions"`
		SHAPinningRequired bool   `json:"sha_pinning_required"`
	}
)

// fetched is one decoded response: present (200), absent (404) or not collected (anything else).
type fetched[T any] struct {
	value  *T
	absent bool
	reason string
}

func get[T any](in githubInput, name string) fetched[T] {
	r, ok := in.Responses[name]
	switch {
	case !ok:
		return fetched[T]{reason: "no " + name + " response recorded"}
	case r.Status == 200:
		var v T
		if err := json.Unmarshal(r.Body, &v); err != nil {
			return fetched[T]{reason: fmt.Sprintf("%s: unexpected response: %v", name, err)}
		}
		return fetched[T]{value: &v}
	case r.Status == 404:
		return fetched[T]{absent: true}
	case r.Status == 0:
		return fetched[T]{reason: cmp.Or(r.Reason, name+" not requested")}
	default:
		return fetched[T]{reason: fmt.Sprintf("%s: HTTP %d", name, r.Status)}
	}
}

func parseGitHubSettings(data []byte) (*githubData, bool, string, error) {
	var in githubInput
	if err := decode(data, &in); err != nil {
		return nil, false, "", err
	}
	if in.Responses == nil {
		return nil, false, "", errors.New("responses is missing")
	}

	env := get[ghEnvironment](in, "environment")
	policies := get[ghBranchPolicies](in, "environmentBranchPolicies")
	branch := get[ghBranch](in, "branch")
	rules := get[[]ghRule](in, "branchRules")
	rulesets := get[[]ghRuleset](in, "rulesets")
	protection := get[ghProtection](in, "branchProtection")
	workflow := get[ghWorkflowPermissions](in, "workflowPermissions")
	actions := get[ghActionsPermissions](in, "actionsPermissions")

	checks := []githubCheck{
		envExists(env),
		envReviewers(env),
		envBranches(env, policies),
		mainPullRequest(branch, rules, protection),
		mainRequiredCheck(branch, rules),
		mainHistory(branch, rules, protection),
		releaseTags(rulesets),
		workflowReadOnly(workflow),
		workflowNoApprovals(workflow),
		actionsSHAPinning(actions),
	}

	d := &githubData{Checks: checks}
	var missing []string
	attention := false
	for _, c := range checks {
		switch c.Status {
		case checkMissing:
			if c.Required {
				d.Missing++
				missing = append(missing, c.Control)
				attention = true
			}
		case checkNotCollected:
			d.NotCollected++
			// With a token that should have worked, a gap is a failure, not a choice.
			attention = attention || (c.Required && in.AdminToken)
		}
	}

	required := 0
	ok := 0
	for _, c := range checks {
		if c.Required {
			required++
			if c.Status == checkOK {
				ok++
			}
		}
	}
	headline := fmt.Sprintf("%d of %d required controls in place", ok, required)
	if len(missing) > 0 {
		headline += "; missing: " + strings.Join(missing, "; ")
	}
	if d.NotCollected > 0 {
		headline += fmt.Sprintf("; %s not collected", plural(d.NotCollected, "check", "checks"))
		if !in.AdminToken {
			headline += " (no token with Administration read)"
		}
	}
	return d, attention, headline, nil
}

func check(id, control string, required bool) githubCheck {
	return githubCheck{ID: id, Control: control, Required: required}
}

func (c githubCheck) ok(detail string) githubCheck {
	c.Status, c.Detail = checkOK, detail
	return c
}

func (c githubCheck) missing(detail string) githubCheck {
	c.Status, c.Detail = checkMissing, detail
	return c
}

func (c githubCheck) unknown(reason string) githubCheck {
	c.Status, c.Detail = checkNotCollected, reason
	return c
}

func envExists(env fetched[ghEnvironment]) githubCheck {
	c := check("lab-environment", "environment lab exists", true)
	switch {
	case env.value != nil:
		return c.ok("present")
	case env.absent:
		return c.missing("no environment named " + applyEnvironment)
	default:
		return c.unknown(env.reason)
	}
}

func envReviewers(env fetched[ghEnvironment]) githubCheck {
	c := check("lab-reviewers", "deployments to lab wait for a reviewer", true)
	switch {
	case env.absent:
		return c.missing("no environment named " + applyEnvironment)
	case env.value == nil:
		return c.unknown(env.reason)
	}
	for _, r := range env.value.ProtectionRules {
		if r.Type == "required_reviewers" && len(r.Reviewers) > 0 {
			detail := plural(len(r.Reviewers), "reviewer", "reviewers")
			if b := env.value.CanAdminsBypass; b != nil && *b {
				detail += "; admins may bypass"
			}
			return c.ok(detail)
		}
	}
	return c.missing("no required reviewers")
}

func envBranches(env fetched[ghEnvironment], policies fetched[ghBranchPolicies]) githubCheck {
	c := check("lab-branches", "only main can deploy to lab", true)
	switch {
	case env.absent:
		return c.missing("no environment named " + applyEnvironment)
	case env.value == nil:
		return c.unknown(env.reason)
	}
	p := env.value.DeploymentBranchPolicy
	switch {
	case p == nil:
		return c.missing("any branch can deploy")
	case p.ProtectedBranches:
		return c.ok("protected branches only")
	case !p.CustomBranchPolicies:
		return c.missing("no branch rule")
	case policies.value == nil:
		return c.unknown(cmp.Or(policies.reason, "branch policies not found"))
	}
	var names []string
	for _, b := range policies.value.BranchPolicies {
		names = append(names, b.Type+":"+b.Name)
	}
	slices.Sort(names)
	if slices.Equal(names, []string{"branch:main"}) {
		return c.ok("branch main only")
	}
	return c.missing("allowed: " + cmp.Or(strings.Join(names, ", "), "nothing"))
}

// ruleTypes returns the rule types that apply to main.
func ruleTypes(rules fetched[[]ghRule]) []string {
	var types []string
	if rules.value != nil {
		for _, r := range *rules.value {
			types = append(types, r.Type)
		}
	}
	return types
}

func mainPullRequest(branch fetched[ghBranch], rules fetched[[]ghRule], protection fetched[ghProtection]) githubCheck {
	c := check("main-pull-request", "changes reach main only through a pull request", true)
	switch {
	case slices.Contains(ruleTypes(rules), "pull_request"):
		return c.ok("ruleset rule pull_request")
	case protection.value != nil && protection.value.RequiredPullRequestReviews != nil:
		return c.ok("branch protection requires pull request reviews")
	case rules.value == nil && !rules.absent:
		return c.unknown(rules.reason)
	case branch.value != nil && branch.value.Protected && protection.value == nil && !protection.absent:
		return c.unknown("main has branch protection; its details need Administration read (" + protection.reason + ")")
	}
	return c.missing("no ruleset or branch protection requires a pull request")
}

func mainRequiredCheck(branch fetched[ghBranch], rules fetched[[]ghRule]) githubCheck {
	c := check("main-ci-result", "the "+requiredCheckName+" check must pass before merging", true)
	if rules.value != nil {
		for _, r := range *rules.value {
			if r.Type != "required_status_checks" {
				continue
			}
			var p struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			}
			if json.Unmarshal(r.Parameters, &p) == nil {
				for _, s := range p.RequiredStatusChecks {
					if s.Context == requiredCheckName {
						return c.ok("ruleset rule required_status_checks")
					}
				}
			}
		}
	}
	switch {
	case branch.value != nil && slices.Contains(branch.value.Protection.RequiredStatusChecks.Contexts, requiredCheckName):
		return c.ok("branch protection required check")
	case rules.value == nil && branch.value == nil:
		return c.unknown(cmp.Or(rules.reason, branch.reason))
	}
	return c.missing(requiredCheckName + " is not a required check on main")
}

func mainHistory(branch fetched[ghBranch], rules fetched[[]ghRule], protection fetched[ghProtection]) githubCheck {
	c := check("main-history", "main can't be force-pushed or deleted", true)
	types := ruleTypes(rules)
	if slices.Contains(types, "non_fast_forward") && slices.Contains(types, "deletion") {
		return c.ok("ruleset rules non_fast_forward and deletion")
	}
	if p := protection.value; p != nil {
		if !p.AllowForcePushes.Enabled && !p.AllowDeletions.Enabled {
			return c.ok("branch protection blocks force pushes and deletion")
		}
		return c.missing("branch protection allows force pushes or deletion")
	}
	if branch.value != nil && branch.value.Protected && !protection.absent {
		return c.unknown("main has branch protection; its details need Administration read (" + protection.reason + ")")
	}
	if rules.value == nil && !rules.absent {
		return c.unknown(rules.reason)
	}
	var lacking []string
	for _, t := range []string{"non_fast_forward", "deletion"} {
		if !slices.Contains(types, t) {
			lacking = append(lacking, t)
		}
	}
	return c.missing("no rule " + strings.Join(lacking, " or "))
}

func releaseTags(rulesets fetched[[]ghRuleset]) githubCheck {
	c := check("release-tags", "release tags can't be moved or deleted", true)
	if rulesets.value == nil {
		if rulesets.absent {
			return c.missing("no rulesets")
		}
		return c.unknown(rulesets.reason)
	}
	for _, r := range *rulesets.value {
		if r.Target != "tag" || r.Enforcement != "active" {
			continue
		}
		var types []string
		for _, rule := range r.Rules {
			types = append(types, rule.Type)
		}
		if slices.Contains(types, "deletion") && (slices.Contains(types, "update") || slices.Contains(types, "non_fast_forward")) {
			return c.ok("tag ruleset " + r.Name)
		}
	}
	return c.missing("no active tag ruleset that blocks deleting and moving tags")
}

func workflowReadOnly(w fetched[ghWorkflowPermissions]) githubCheck {
	c := check("workflow-token-read", "workflows get a read-only GITHUB_TOKEN by default", true)
	switch {
	case w.value == nil:
		return c.unknown(w.reason)
	case w.value.DefaultWorkflowPermissions == "read":
		return c.ok("default read")
	}
	return c.missing("default " + cmp.Or(w.value.DefaultWorkflowPermissions, "unknown"))
}

func workflowNoApprovals(w fetched[ghWorkflowPermissions]) githubCheck {
	c := check("workflow-no-approvals", "workflows can't approve pull requests", true)
	switch {
	case w.value == nil:
		return c.unknown(w.reason)
	case !w.value.CanApprovePullRequestReviews:
		return c.ok("not allowed")
	}
	return c.missing("allowed")
}

func actionsSHAPinning(a fetched[ghActionsPermissions]) githubCheck {
	// Every action here is pinned by SHA in review anyway; the setting makes GitHub enforce it.
	c := check("actions-sha-pinning", "GitHub enforces actions pinned to a full commit SHA", false)
	switch {
	case a.value == nil:
		return c.unknown(a.reason)
	case a.value.SHAPinningRequired:
		return c.ok("required")
	}
	return c.missing("not required (allowed actions: " + cmp.Or(a.value.AllowedActions, "unknown") + ")")
}
