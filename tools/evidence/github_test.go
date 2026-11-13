package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// settings builds a github-settings.json with the given responses, each "status" or "status body".
func settings(t *testing.T, adminToken bool, responses map[string]any) []byte {
	t.Helper()
	in := map[string]any{"adminToken": adminToken, "responses": responses}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func resp(status int, body any) map[string]any {
	return map[string]any{"status": status, "body": body}
}

func statuses(d *githubData) map[string]string {
	m := map[string]string{}
	for _, c := range d.Checks {
		m[c.ID] = c.Status
	}
	return m
}

func TestGitHubSettingsSample(t *testing.T) {
	data, err := os.ReadFile("testdata/inputs/github-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	d, attention, headline, err := parseGitHubSettings(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"lab-environment":       checkOK,
		"lab-reviewers":         checkOK,
		"lab-branches":          checkOK,
		"main-pull-request":     checkOK,
		"main-ci-result":        checkOK,
		"main-history":          checkOK,
		"release-tags":          checkMissing, // the tag ruleset is only in evaluate mode
		"workflow-token-read":   checkNotCollected,
		"workflow-no-approvals": checkNotCollected,
		"actions-sha-pinning":   checkNotCollected,
	}
	if got := statuses(d); !mapsEqual(got, want) {
		t.Errorf("checks = %v\nwant %v", got, want)
	}
	if !attention || d.Missing != 1 || d.NotCollected != 3 {
		t.Errorf("attention=%v missing=%d notCollected=%d", attention, d.Missing, d.NotCollected)
	}
	wantHeadline := "6 of 9 required controls in place; missing: release tags can't be moved or deleted; 3 checks not collected (no token with Administration read)"
	if headline != wantHeadline {
		t.Errorf("headline = %q\nwant %q", headline, wantHeadline)
	}
}

func TestGitHubSettingsGaps(t *testing.T) {
	noToken := map[string]any{"status": 0, "reason": "no token"}
	tests := []struct {
		name      string
		admin     bool
		responses map[string]any
		want      map[string]string
		attention bool
	}{
		{
			name: "nothing set up yet",
			responses: map[string]any{
				"environment":         resp(404, map[string]any{"message": "Not Found"}),
				"branch":              resp(200, map[string]any{"protected": false}),
				"branchRules":         resp(200, []any{}),
				"rulesets":            resp(200, []any{}),
				"branchProtection":    noToken,
				"workflowPermissions": noToken,
			},
			want: map[string]string{
				"lab-environment": checkMissing, "lab-reviewers": checkMissing, "lab-branches": checkMissing,
				"main-pull-request": checkMissing, "main-ci-result": checkMissing, "main-history": checkMissing,
				"release-tags": checkMissing, "workflow-token-read": checkNotCollected,
				"workflow-no-approvals": checkNotCollected, "actions-sha-pinning": checkNotCollected,
			},
			attention: true,
		},
		{
			name:  "classic protection read with an admin token, environment without rules",
			admin: true,
			responses: map[string]any{
				"environment": resp(200, map[string]any{"protection_rules": []any{}, "deployment_branch_policy": nil}),
				"branch": resp(200, map[string]any{"protected": true, "protection": map[string]any{
					"required_status_checks": map[string]any{"contexts": []string{"CI result"}}}}),
				"branchRules": resp(200, []any{}),
				"rulesets": resp(200, []any{map[string]any{"name": "tags", "target": "tag", "enforcement": "active",
					"rules": []any{map[string]any{"type": "deletion"}, map[string]any{"type": "non_fast_forward"}}}}),
				"branchProtection": resp(200, map[string]any{"required_pull_request_reviews": map[string]any{},
					"allow_force_pushes": map[string]any{"enabled": true}, "allow_deletions": map[string]any{"enabled": false}}),
				"workflowPermissions": resp(200, map[string]any{"default_workflow_permissions": "write", "can_approve_pull_request_reviews": true}),
				"actionsPermissions":  resp(200, map[string]any{"allowed_actions": "all", "sha_pinning_required": true}),
			},
			want: map[string]string{
				"lab-environment": checkOK, "lab-reviewers": checkMissing, "lab-branches": checkMissing,
				"main-pull-request": checkOK, "main-ci-result": checkOK, "main-history": checkMissing,
				"release-tags": checkOK, "workflow-token-read": checkMissing,
				"workflow-no-approvals": checkMissing, "actions-sha-pinning": checkOK,
			},
			attention: true,
		},
		{
			name:  "everything in place, protected-branches policy",
			admin: true,
			responses: map[string]any{
				"environment": resp(200, map[string]any{
					"protection_rules":         []any{map[string]any{"type": "required_reviewers", "reviewers": []any{map[string]any{"type": "User"}}}},
					"deployment_branch_policy": map[string]any{"protected_branches": true},
					"can_admins_bypass":        true,
				}),
				"branch": resp(200, map[string]any{"protected": true}),
				"branchRules": resp(200, []any{
					map[string]any{"type": "pull_request"}, map[string]any{"type": "non_fast_forward"}, map[string]any{"type": "deletion"},
					map[string]any{"type": "required_status_checks", "parameters": map[string]any{
						"required_status_checks": []any{map[string]any{"context": "CI result"}}}},
				}),
				"rulesets": resp(200, []any{map[string]any{"name": "tags", "target": "tag", "enforcement": "active",
					"rules": []any{map[string]any{"type": "deletion"}, map[string]any{"type": "update"}}}}),
				"branchProtection":    resp(404, nil),
				"workflowPermissions": resp(200, map[string]any{"default_workflow_permissions": "read"}),
				"actionsPermissions":  resp(200, map[string]any{"allowed_actions": "selected"}),
			},
			want: map[string]string{
				"lab-environment": checkOK, "lab-reviewers": checkOK, "lab-branches": checkOK,
				"main-pull-request": checkOK, "main-ci-result": checkOK, "main-history": checkOK,
				"release-tags": checkOK, "workflow-token-read": checkOK,
				"workflow-no-approvals": checkOK, "actions-sha-pinning": checkMissing, // recommended only
			},
			attention: false,
		},
		{
			name:  "an admin token that fails is a gap to look at",
			admin: true,
			responses: map[string]any{
				"environment": resp(200, map[string]any{
					"protection_rules":         []any{map[string]any{"type": "required_reviewers", "reviewers": []any{map[string]any{}}}},
					"deployment_branch_policy": map[string]any{"custom_branch_policies": true},
				}),
				"environmentBranchPolicies": resp(200, map[string]any{"branch_policies": []any{
					map[string]any{"name": "main", "type": "branch"}, map[string]any{"name": "feat/*", "type": "branch"}}}),
				"branch":              resp(500, nil),
				"branchRules":         resp(403, nil),
				"rulesets":            resp(500, nil),
				"branchProtection":    resp(403, nil),
				"workflowPermissions": resp(403, nil),
				"actionsPermissions":  resp(200, "not an object"),
			},
			want: map[string]string{
				"lab-environment": checkOK, "lab-reviewers": checkOK, "lab-branches": checkMissing,
				"main-pull-request": checkNotCollected, "main-ci-result": checkNotCollected, "main-history": checkNotCollected,
				"release-tags": checkNotCollected, "workflow-token-read": checkNotCollected,
				"workflow-no-approvals": checkNotCollected, "actions-sha-pinning": checkNotCollected,
			},
			attention: true,
		},
		{
			name: "custom policy not readable, protected main without details",
			responses: map[string]any{
				"environment": resp(200, map[string]any{
					"protection_rules":         []any{},
					"deployment_branch_policy": map[string]any{"custom_branch_policies": true},
				}),
				"branch":           resp(200, map[string]any{"protected": true}),
				"branchRules":      resp(200, []any{}),
				"rulesets":         resp(404, nil),
				"branchProtection": noToken,
			},
			want: map[string]string{
				"lab-environment": checkOK, "lab-reviewers": checkMissing, "lab-branches": checkNotCollected,
				"main-pull-request": checkNotCollected, "main-ci-result": checkMissing, "main-history": checkNotCollected,
				"release-tags": checkMissing, "workflow-token-read": checkNotCollected,
				"workflow-no-approvals": checkNotCollected, "actions-sha-pinning": checkNotCollected,
			},
			attention: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, attention, _, err := parseGitHubSettings(settings(t, tc.admin, tc.responses))
			if err != nil {
				t.Fatal(err)
			}
			if got := statuses(d); !mapsEqual(got, tc.want) {
				t.Errorf("checks = %v\nwant %v", got, tc.want)
			}
			if attention != tc.attention {
				t.Errorf("attention = %v, want %v", attention, tc.attention)
			}
		})
	}
}

func TestGitHubSettingsInvalid(t *testing.T) {
	for _, bad := range []string{`[]`, `{}`, `{"responses": 1}`} {
		if _, _, _, err := parseGitHubSettings([]byte(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	d, _, headline, err := parseGitHubSettings([]byte(`{"responses": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.NotCollected != len(d.Checks) || !strings.Contains(headline, "0 of 9 required controls in place") {
		t.Errorf("an empty record collects nothing: %+v %q", d, headline)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
