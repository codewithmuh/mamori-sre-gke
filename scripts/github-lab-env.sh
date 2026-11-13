#!/usr/bin/env bash
# Creates or updates the GitHub environment `lab`, the only place allowed to impersonate tf-apply:
# a required reviewer, and deployments from main only. Without this, GitHub would create the
# environment unprotected the first time a workflow names it. `make bootstrap` runs it; safe to
# run again.
#
# Usage: scripts/github-lab-env.sh
#   GITHUB_REPO   default codewithmuh/mamori-sre-gke
#   LAB_REVIEWER  GitHub login that approves applies (default: the repository owner)
# Needs gh, logged in with admin rights on the repository.
set -euo pipefail

repo=${GITHUB_REPO:-codewithmuh/mamori-sre-gke}
reviewer=${LAB_REVIEWER:-${repo%%/*}}

if ! command -v gh >/dev/null 2>&1; then
  echo "error: gh is not installed" >&2
  exit 1
fi

reviewer_id=$(gh api "users/${reviewer}" --jq .id)

# prevent_self_review stays false: with one maintainer, the person who starts the apply is also
# the only one who can approve it.
gh api --method PUT "repos/${repo}/environments/lab" --input - >/dev/null <<EOF
{
  "wait_timer": 0,
  "prevent_self_review": false,
  "reviewers": [{"type": "User", "id": ${reviewer_id}}],
  "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}
}
EOF

policies=$(gh api "repos/${repo}/environments/lab/deployment-branch-policies" \
  --jq '.branch_policies[] | "\(.type // "branch"):\(.name)"')
if ! grep -qx 'branch:main' <<<"$policies"; then
  gh api --method POST "repos/${repo}/environments/lab/deployment-branch-policies" \
    -f name=main -f type=branch >/dev/null
fi
others=$(grep -vx -e 'branch:main' -e '' <<<"$policies" || true)
if [[ -n $others ]]; then
  echo "warning: environment lab also allows deployments from: $(tr '\n' ' ' <<<"$others")" >&2
  echo "         remove them under Settings > Environments > lab unless you added them on purpose." >&2
fi

echo "GitHub environment lab in ${repo}: required reviewer ${reviewer}, deployments from main only."
