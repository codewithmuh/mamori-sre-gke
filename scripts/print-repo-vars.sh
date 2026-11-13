#!/usr/bin/env bash
# Prints the GitHub repository variables CI needs, read from the infra/bootstrap outputs, as `gh`
# commands to paste, plus the secrets and settings to add by hand. It sets nothing itself.
# `make bootstrap` runs it last, after creating the lab environment and applying infra/bootstrap.
#
# Usage: scripts/print-repo-vars.sh   (needs PROJECT_ID; BOOTSTRAP_DIR defaults to infra/bootstrap)
set -euo pipefail

project=${PROJECT_ID:?PROJECT_ID is not set}
dir=${BOOTSTRAP_DIR:-infra/bootstrap}
repo=${GITHUB_REPO:-codewithmuh/mamori-sre-gke}

output() {
  terraform -chdir="$dir" output -raw "$1"
}

wif_provider=$(output wif_provider)
tf_plan_sa=$(output tf_plan_sa)
tf_apply_sa=$(output tf_apply_sa)
state_bucket=$(output state_bucket)

cat <<EOF

GitHub settings for ${repo}. Paste these commands (needs gh with admin rights on the repo),
or set the same values under Settings > Secrets and variables > Actions.

Repository variables (not secret; CI prints them in public logs):
EOF
for pair in \
  "GCP_PROJECT_ID=${project}" \
  "GCP_WIF_PROVIDER=${wif_provider}" \
  "GCP_TF_PLAN_SA=${tf_plan_sa}" \
  "GCP_TF_APPLY_SA=${tf_apply_sa}" \
  "TF_STATE_BUCKET=${state_bucket}"; do
  printf '  gh variable set %s --repo %s --body %q\n' "${pair%%=*}" "$repo" "${pair#*=}"
done

cat <<EOF

Repository secret (a secret, so the billing account ID is masked in public logs):
  gh secret set GCP_BILLING_ACCOUNT_ID --repo ${repo}    # paste the ID when asked

Environment "lab": \`make bootstrap\` has just created or updated it (scripts/github-lab-env.sh)
with you as required reviewer and deployments from main only. Only jobs in that environment, on
main, may impersonate tf-apply. Check it under Settings > Environments > lab.

Release PRs (release-please), pick one:
  a) Recommended: a token whose pull requests trigger CI, so the Release PR gets "CI result":
     a GitHub App installation token, or a fine-grained token for ${repo} only with
     Contents and Pull requests read/write (set an expiry and a reminder to renew it):
       gh secret set RELEASE_PLEASE_TOKEN --repo ${repo}
  b) No token: turn on Settings > Actions > General > "Allow GitHub Actions to create and
     approve pull requests". CI then does not run on the Release PR, so a required
     "CI result" check blocks it until you close and reopen the PR yourself.
EOF
