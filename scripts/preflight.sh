#!/usr/bin/env bash
# Read-only checks to run before each step of the lab's first run (docs/runbooks/first-run.md,
# issue #15). It never creates or changes anything: it runs `gcloud` describe, list and
# get-iam-policy calls, `gh api` GET requests, `kubectl get`, DNS lookups and one HTTPS GET of the
# shop, and reads files in this checkout. gcloud prompts are off, so it can't enable an API either.
#
# Usage: scripts/preflight.sh [--phase before-bootstrap|before-up|after-up]   (default: before-up)
#   before-bootstrap  tools, logins, inputs, the project and its billing account, the GitHub OIDC
#                     subject that bootstrap writes into the identity pool, quotas if readable
#   before-up         the above, plus the state bucket, the bootstrap state, the APIs, the
#                     region's CPU and address quotas, the GitHub variables, secrets and
#                     environment `lab`, and every input `make up` needs
#   after-up          the cluster, nodes, Argo CD, the Gateway, the static IP and DNS records, the
#                     certificate, the shop and its banner, the Workload Identity grants (second
#                     bootstrap), our images and the prober, the SLO switch, the spend guard, the
#                     budget and the quotas to write down
#
# Settings: PROJECT_ID, ZONE, BILLING_ACCOUNT_ID, FRONTEND_HOSTNAME, TF_STATE_BUCKET, SLO_ENABLED and
# ALERT_EMAIL come from the environment, else from .env, with the Makefile's defaults
# (TF_VAR_slo_enabled and TF_VAR_alert_email in the environment count too). Optional: GITHUB_REPO (default
# codewithmuh/mamori-sre-gke), DNS_SERVER (ask this resolver, e.g. 1.1.1.1, instead of the system's),
# ENV_FILE (default .env in the repository root), NO_COLOR.
#
# Output: one PASS, WARN, FAIL, SKIP or INFO line per check; an indented line under it says what to
# do. Secret values are never printed: GitHub variables and secrets are checked by name, Terraform
# inputs only for being set, and the billing account ID and your e-mail address are masked.
#
# Exit status: 0 no FAIL (WARN lines may be there), 1 at least one FAIL, 2 bad usage.
set -euo pipefail

usage() {
  sed -n '2,/^set -euo/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'
}

phase=before-up
while (($#)); do
  case $1 in
    --phase)
      [[ $# -ge 2 ]] || { echo "error: --phase needs a value" >&2; exit 2; }
      phase=$2
      shift
      ;;
    --phase=*) phase=${1#--phase=} ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done
case $phase in
  before-bootstrap | before-up | after-up) ;;
  *)
    echo "error: --phase must be before-bootstrap, before-up or after-up (got '${phase}')" >&2
    exit 2
    ;;
esac

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
env_file=${ENV_FILE:-$root/.env}
repo=${GITHUB_REPO:-codewithmuh/mamori-sre-gke}
bootstrap_dir=$root/infra/bootstrap
lab_dir=$root/infra/envs/lab

# Names the Terraform and the manifests use (infra/README.md, "Names other parts depend on").
cluster=mamori
node_pool=spot-pool
frontend_ip_name=mamori-frontend-ip
dnsauth_name=mamori-frontend-dnsauth
cert_name=mamori-frontend-cert
router_name=mamori-router
# 3 x e2-standard-2 Spot nodes (2 vCPU each) plus one surge node during upgrades (gke.tf).
node_vcpus=2
plan_vcpus=8
# Kubernetes minor the manifests are validated against (deploy/README.md); kubectl may be one off.
cluster_minor=36

export CLOUDSDK_CORE_DISABLE_PROMPTS=1 # never stop to ask, for example to enable an API
errfile=$(mktemp)
bodyfile=$(mktemp)
trap 'rm -f "$errfile" "$bodyfile"' EXIT

if [[ -t 1 && -z ${NO_COLOR:-} ]]; then
  red=$'\033[31m' green=$'\033[32m' yellow=$'\033[33m' dim=$'\033[2m' reset=$'\033[0m'
else
  red='' green='' yellow='' dim='' reset=''
fi

npass=0 nwarn=0 nfail=0
records=''

# report LEVEL MESSAGE [HINT...]: one result line, then each hint indented below it.
report() {
  local level=$1 msg=$2 color='' hint
  shift 2
  case $level in
    PASS) npass=$((npass + 1)) color=$green ;;
    WARN) nwarn=$((nwarn + 1)) color=$yellow ;;
    FAIL) nfail=$((nfail + 1)) color=$red ;;
    *) color=$dim ;;
  esac
  printf '%s%-4s%s  %s\n' "$color" "$level" "$reset" "$msg"
  for hint in "$@"; do
    [[ -n $hint ]] || continue
    printf '      %s\n' "$hint"
  done
}
pass() { report PASS "$@"; }
warn() { report WARN "$@"; }
fail() { report FAIL "$@"; }
skip() { report SKIP "$@"; }
info() { report INFO "$@"; }

# todo MESSAGE [HINT...]: a step of the runbook not done yet. WARN before `make up` (the lab doesn't
# need it), FAIL after it (the first run isn't finished).
todo() {
  if [[ $phase == after-up ]]; then fail "$@"; else warn "$@"; fi
}

section() {
  printf '\n== %s\n' "$1"
}

record() {
  records+="  $1"$'\n'
}

have() {
  command -v "$1" >/dev/null 2>&1
}

# gc ARGS...: run gcloud; stdout in $out, the most useful stderr line in $err. Returns its status.
gc() {
  local status=0
  out=$(gcloud "$@" 2>"$errfile") || status=$?
  err=$(grep -m1 -E '^ERROR' "$errfile" || head -n 1 "$errfile" || true)
  err=${err:0:300}
  return "$status"
}

# ghapi PATH [ARGS...]: GET a GitHub API path; stdout in $out, the first stderr line in $err.
ghapi() {
  local path=$1 status=0
  shift
  out=$(gh api --method GET "$path" "$@" 2>"$errfile") || status=$?
  err=$(head -n 1 "$errfile" || true)
  return "$status"
}

# kc ARGS...: run kubectl with a timeout; stdout in $out, the first stderr line in $err.
kc() {
  local status=0
  out=$(kubectl --request-timeout=20s "$@" 2>"$errfile") || status=$?
  err=$(head -n 1 "$errfile" || true)
  return "$status"
}

# ver_ge A B: true when dotted version A >= B (first three numeric parts).
ver_ge() {
  local a b i x y
  IFS=. read -r -a a <<<"$1"
  IFS=. read -r -a b <<<"$2"
  for i in 0 1 2; do
    x=${a[i]:-0} y=${b[i]:-0}
    x=${x%%[!0-9]*} y=${y%%[!0-9]*}
    x=$((10#${x:-0})) y=$((10#${y:-0}))
    if ((x > y)); then return 0; fi
    if ((x < y)); then return 1; fi
  done
  return 0
}

# env_file_value KEY: the last value of KEY in .env (KEY=value, as make reads it), trimmed.
env_file_value() {
  [[ -f $env_file ]] || return 0
  awk -v key="$1" '
    /^[[:space:]]*#/ { next }
    match($0, "^[[:space:]]*" key "[[:space:]]*[:?]?=") { val = substr($0, RLENGTH + 1) }
    END { gsub(/^[[:space:]]+|[[:space:]]+$/, "", val); print val }' "$env_file"
}

# setting NAME DEFAULT: sets $val (non-empty environment, then .env, then DEFAULT) and $src.
setting() {
  local name=$1 default=$2
  val=${!name:-} src=environment
  if [[ -z $val ]]; then val=$(env_file_value "$name") src=.env; fi
  if [[ -z $val ]]; then val=$default src=default; fi
}

# note_default SOURCE: " (default)" when a setting fell back to the Makefile's default.
note_default() {
  if [[ $1 == default ]]; then printf ' (default)'; fi
}

mask_email() {
  if [[ $1 == *@* ]]; then printf '%s...@%s' "${1:0:1}" "${1#*@}"; else printf '%s' "$1"; fi
}

# required_tf_vars DIR: variables of a Terraform root that have no default, one per line.
required_tf_vars() {
  local f
  for f in "$1"/*.tf; do
    [[ -f $f ]] || continue
    awk '
      /^variable "[^"]+"/ { split($0, p, "\""); name = p[2]; invar = 1; hasdef = 0; next }
      invar && /^[[:space:]]+default[[:space:]]*=/ { hasdef = 1 }
      invar && /^}/ { if (!hasdef) print name; invar = 0 }' "$f"
  done
}

# tfvars_sets DIR NAME: true when terraform.tfvars or a *.auto.tfvars file in DIR sets NAME.
tfvars_sets() {
  local f
  for f in "$1"/terraform.tfvars "$1"/*.auto.tfvars; do
    [[ -f $f ]] || continue
    if grep -Eq "^[[:space:]]*$2[[:space:]]*=" "$f"; then return 0; fi
  done
  return 1
}

# tf_default FILE VARIABLE: the default of a string variable in a .tf file.
tf_default() {
  awk -v v="$2" '
    $0 ~ "^variable \"" v "\"" { invar = 1; next }
    invar && /^[[:space:]]+default[[:space:]]*=/ { split($0, p, "\""); print p[2]; exit }
    invar && /^}/ { exit }' "$1"
}

# registry_placeholders: files under deploy/ that still name the registry project PROJECT_ID
# (scripts/set-registry.sh replaces it), one per line. The policy test cases keep it on purpose.
registry_placeholders() {
  grep -rl --include='*.yaml' 'docker\.pkg\.dev/PROJECT_ID/' "$root/deploy" 2>/dev/null |
    grep -v '/deploy/policy/tests/' | sed "s|^$root/||" || true
}

# lookup TYPE NAME: DNS answers, one per line, lower case, without the trailing dot.
lookup() {
  local raw
  if have dig; then
    raw=$(dig +short +time=3 +tries=2 ${DNS_SERVER:+"@$DNS_SERVER"} "$1" "$2" 2>/dev/null || true)
  else
    raw=$(host -W 3 -t "$1" "$2" ${DNS_SERVER:+"$DNS_SERVER"} 2>/dev/null |
      awk '/has address|is an alias for/ { print $NF }' || true)
  fi
  printf '%s\n' "$raw" | tr '[:upper:]' '[:lower:]' | sed -e 's/\.$//' -e '/^$/d'
}

# quota DATA METRIC: "limit usage" of METRIC from gcloud csv lines (metric,limit,usage).
quota() {
  awk -F, -v m="$2" '$1 == m { print $2, $3; found = 1; exit } END { exit !found }' <<<"$1"
}

num() {
  local n=$1
  printf '%s' "${n%.0}"
}

# check_quota LABEL DATA METRIC NEED SEVERITY: PASS when limit - usage >= NEED.
check_quota() {
  local label=$1 data=$2 metric=$3 need=$4 sev=$5 lu limit usage free msg
  if ! lu=$(quota "$data" "$metric"); then
    skip "$label $metric: not in the quota list" "check Console > IAM & Admin > Quotas"
    return 0
  fi
  read -r limit usage <<<"$lu"
  free=$(awk -v l="$limit" -v u="$usage" 'BEGIN { print l - u }')
  record "$label $metric: limit $(num "$limit"), in use $(num "$usage")"
  msg="$label $metric: $(num "$free") free of $(num "$limit"), the plan needs $need more"
  if ((need == 0)); then msg="$label $metric: $(num "$free") free of $(num "$limit"), the lab already holds what it needs"; fi
  if awk -v f="$free" -v n="$need" 'BEGIN { exit !(f >= n) }'; then
    pass "$msg"
  else
    report "$sev" "$msg" \
      "a trial account can't request more; see docs/runbooks/first-run.md, 'Quota'"
  fi
}

# --- Settings ----------------------------------------------------------------------------------

setting PROJECT_ID ''
project=$val
setting ZONE asia-northeast1-a
zone=$val zone_src=$src
setting BILLING_ACCOUNT_ID ''
billing=$val
setting FRONTEND_HOSTNAME mamori.codewithmuh.com
hostname=$val host_src=$src
setting TF_STATE_BUCKET "${project:+$project-tfstate}"
bucket=$val bucket_src=$src
# The Makefile exports these two as TF_VAR_slo_enabled and TF_VAR_alert_email when they are set.
setting SLO_ENABLED "${TF_VAR_slo_enabled:-}"
slo_setting=$val
setting ALERT_EMAIL "${TF_VAR_alert_email:-}"
alert_email=$val
region=${zone%-*}

printf 'mamori preflight: phase %s, project %s, zone %s, repository %s\n' \
  "$phase" "${project:-<unset>}" "$zone" "$repo"

gcloud_ok=0 gcloud_auth=0 gh_ok=0 kubectl_ok=0 project_ok=0 account=''
enabled_apis='' compute_on=0 billing_ok=0 lab_nodes=0 gh_vars='' gh_vars_ok=0 edge_off=0

# --- Local tools -------------------------------------------------------------------------------

section "Local tools"
if have gcloud; then
  v=$(gcloud version 2>/dev/null | sed -n 's/^Google Cloud SDK //p' | head -n 1 || true)
  pass "gcloud ${v:-(version unknown)}"
  gcloud_ok=1
else
  fail "gcloud not found" "install the Google Cloud CLI (runbook step 2), or work in the dev container"
fi
if have gke-gcloud-auth-plugin; then
  pass "gke-gcloud-auth-plugin on PATH"
else
  fail "gke-gcloud-auth-plugin not on PATH (kubectl needs it for GKE)" \
    "gcloud components install gke-gcloud-auth-plugin"
fi
if have terraform; then
  v=$(terraform version -json 2>/dev/null | sed -n 's/.*"terraform_version": *"\([^"]*\)".*/\1/p' | head -n 1 || true)
  if [[ -z $v ]]; then v=$(terraform version 2>/dev/null | sed -n '1s/^Terraform v//p' || true); fi
  if [[ -n $v ]] && ver_ge "$v" 1.9.0 && ! ver_ge "$v" 2.0.0; then
    pass "terraform $v (needs >= 1.9, < 2)"
  else
    fail "terraform ${v:-(version unknown)}: both roots need >= 1.9.0 and < 2.0.0" \
      "install 1.9.8 (local) or 1.17.x (as CI)"
  fi
else
  fail "terraform not found" "install terraform >= 1.9, < 2"
fi
if have kubectl; then
  kubectl_ok=1
  v=$(kubectl version --client 2>/dev/null | sed -n 's/^Client Version: v//p' | head -n 1 || true)
  minor=$(printf '%s' "$v" | cut -d. -f2)
  if [[ $minor =~ ^[0-9]+$ ]] && ((minor >= cluster_minor - 1 && minor <= cluster_minor + 1)); then
    pass "kubectl $v"
  else
    warn "kubectl ${v:-(version unknown)}: the cluster runs 1.${cluster_minor} (GKE REGULAR); kubectl supports one minor version either way"
  fi
else
  fail "kubectl not found" "install kubectl 1.$((cluster_minor - 1))-1.$((cluster_minor + 1))"
fi
if have helm; then
  v=$(helm version --short 2>/dev/null | sed -e 's/^v//' -e 's/+.*//' || true)
  if [[ -n $v ]] && ver_ge "$v" 3.8.0; then
    pass "helm $v"
  else
    warn "helm ${v:-(version unknown)}: tested with 3.8 and later (the dev container has 4.3.1)"
  fi
else
  fail "helm not found" "make up installs Argo CD with helm"
fi
if have gh; then
  gh_ok=1
  v=$(gh --version 2>/dev/null | sed -n '1s/^gh version \([0-9.]*\).*/\1/p' || true)
  pass "gh ${v:-(version unknown)}"
else
  fail "gh not found" "the GitHub CLI sets the repository variables (make bootstrap prints the commands)"
fi
if have docker; then
  pass "docker (optional) on PATH"
else
  info "docker not found: optional, for local image builds and the offline checks"
fi
if have dig || have host; then
  pass "dig or host for the DNS checks"
else
  warn "neither dig nor host found: the DNS checks are skipped"
fi
if have curl; then
  pass "curl for the HTTPS check"
else
  warn "curl not found: the HTTPS check of the shop is skipped"
fi

# --- Logins ------------------------------------------------------------------------------------

section "Logins"
if ((gcloud_ok)); then
  gc auth list --filter=status:ACTIVE --format='value(account)' || true
  account=$(printf '%s\n' "$out" | head -n 1)
  if gc auth print-access-token && [[ -n $account ]]; then
    pass "gcloud logged in as $(mask_email "$account")"
    gcloud_auth=1
  else
    fail "gcloud is not logged in" "gcloud auth login"
  fi
  out=''
  if gc auth application-default print-access-token; then
    pass "Application Default Credentials (Terraform uses these)"
  else
    fail "no Application Default Credentials" "gcloud auth application-default login"
  fi
  out=''
  gc config get-value project || out=''
  if [[ -n $project && $out == "$project" ]]; then
    pass "gcloud's default project is $project"
  elif [[ -n $project ]]; then
    warn "gcloud's default project is '${out:-<unset>}', not $project: runbook commands without --project use it" \
      "gcloud config set project $project"
  fi
else
  skip "gcloud logins: gcloud not installed"
fi
if ((gh_ok)); then
  if gh auth status >/dev/null 2>&1; then
    pass "gh logged in"
  else
    fail "gh is not logged in" "gh auth login"
    gh_ok=0
  fi
fi

# --- Inputs ------------------------------------------------------------------------------------

section "Inputs (environment, else ${env_file#"$root"/})"
if [[ ! -f $env_file ]]; then
  info "no ${env_file#"$root"/}: settings come from the environment only (template: .env.example)"
fi
project_re='^[a-z][a-z0-9-]{4,28}[a-z0-9]$'
if [[ -z $project ]]; then
  fail "PROJECT_ID is not set" "put it in .env (runbook step 4)"
elif [[ ! $project =~ $project_re ]]; then
  fail "PROJECT_ID '$project' is not a valid project ID (6-30 characters: a-z, 0-9, -)"
  project=''
else
  pass "PROJECT_ID $project"
fi
billing_re='^[0-9A-Z]{6}-[0-9A-Z]{6}-[0-9A-Z]{6}$'
if [[ -z $billing ]]; then
  fail "BILLING_ACCOUNT_ID is not set" "gcloud billing accounts list; put the ID in .env"
elif [[ ! $billing =~ $billing_re ]]; then
  fail "BILLING_ACCOUNT_ID doesn't look like 012345-6789AB-CDEF01"
  billing=''
else
  pass "BILLING_ACCOUNT_ID set (...${billing: -2})"
fi
zone_re='^[a-z]+-[a-z]+[0-9]+-[a-z]$'
if [[ ! $zone =~ $zone_re ]]; then
  fail "ZONE '$zone' doesn't look like asia-northeast1-a"
elif [[ $region != asia-northeast1 ]]; then
  warn "ZONE $zone is outside asia-northeast1: deploy/ pulls our images from asia-northeast1-docker.pkg.dev and the bootstrap buckets are in Tokyo"
else
  pass "ZONE $zone$(note_default "$zone_src")"
fi
deploy_host=$(sed -n 's/^ *- \([a-z0-9.-]*\)$/\1/p' "$root/deploy/edge/httproute.yaml" 2>/dev/null | head -n 1 || true)
if [[ -n $deploy_host && $hostname != "$deploy_host" ]]; then
  warn "FRONTEND_HOSTNAME $hostname differs from $deploy_host in deploy/ (HTTPRoute, Istio Gateway and VirtualService)" \
    "the certificate would be for $hostname while the routes accept only $deploy_host"
else
  pass "FRONTEND_HOSTNAME $hostname$(note_default "$host_src")"
fi
if [[ -n $project ]]; then
  if [[ $bucket != "$project-tfstate" ]]; then
    warn "TF_STATE_BUCKET $bucket: bootstrap always creates $project-tfstate" "leave TF_STATE_BUCKET empty in .env"
  elif [[ $phase == before-bootstrap ]]; then
    info "TF_STATE_BUCKET $bucket (make bootstrap creates it)"
  else
    pass "TF_STATE_BUCKET $bucket$(note_default "$bucket_src")"
  fi
fi

# The lab's alert e-mail is optional: empty means no e-mail channel (infra/envs/lab/notifications.tf).
if grep -qs '^variable "alert_email"' "$lab_dir"/*.tf; then
  if [[ -z $alert_email ]]; then
    info "ALERT_EMAIL is empty: alerts get no e-mail channel (optional; docs/oncall.md)"
  elif [[ $alert_email == *@*.* ]]; then
    pass "ALERT_EMAIL set ($(mask_email "$alert_email"))"
  else
    fail "ALERT_EMAIL doesn't look like an e-mail address"
  fi
fi

# Inputs a root needs that the Makefile doesn't pass: TF_VAR_<name> in the shell (make passes its
# environment through) or a tfvars file in the root.
check_tf_inputs() {
  local dir=$1 label=$2 name var any=0
  while IFS= read -r name; do
    case $name in project_id | billing_account_id | zone | frontend_hostname | '') continue ;; esac
    any=1
    var=TF_VAR_$name
    if [[ -n ${!var:-} ]] || tfvars_sets "$dir" "$name"; then
      pass "$label input $name is set"
    else
      fail "$label input $name has no default and is not set: terraform apply would stop" \
        "export $var=... in the shell before make"
    fi
  done < <(required_tf_vars "$dir")
  if ((!any)); then pass "$label needs no inputs beyond .env"; fi
}

# A terraform.tfvars in a root wins over the TF_VAR_* the Makefile exports.
for dir in "$bootstrap_dir" "$lab_dir"; do
  f=$dir/terraform.tfvars
  [[ -f $f ]] || continue
  tf_project=$(sed -n 's/^[[:space:]]*project_id[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$f" | head -n 1)
  if [[ -n $tf_project && -n $project && $tf_project != "$project" ]]; then
    fail "${f#"$root"/} sets project_id = \"$tf_project\", which overrides PROJECT_ID $project" \
      "remove project_id and billing_account_id from that file (keep only what make doesn't set)"
  fi
done

if [[ $phase == before-bootstrap ]]; then
  check_tf_inputs "$bootstrap_dir" bootstrap
elif [[ $phase == before-up ]]; then
  check_tf_inputs "$lab_dir" lab
  if [[ -d $root/tools/spend-guard ]]; then
    pass "tools/spend-guard is in the checkout (the lab plan zips it)"
  else
    fail "tools/spend-guard is missing: the lab plan zips the function from it"
  fi
  if [[ -n $(registry_placeholders) ]]; then
    info "deploy/ still names the registry project PROJECT_ID: expected until runbook step 15, after make up"
  else
    pass "deploy/ names the real registry project"
  fi
fi

# --- Project and billing ----------------------------------------------------------------------

section "Project and billing"
if ((!gcloud_auth)) || [[ -z $project ]]; then
  skip "project checks: needs gcloud logged in and a valid PROJECT_ID"
else
  if gc projects describe "$project" --format='value(lifecycleState,projectNumber)'; then
    read -r state number <<<"$out"
    if [[ $state == ACTIVE ]]; then
      pass "project $project exists (number $number)"
      project_ok=1
    else
      fail "project $project is $state" "a project pending deletion can be restored for 30 days: gcloud projects undelete $project"
    fi
  else
    fail "project $project not found or not readable" "${err:-}" "create it (runbook step 4)"
  fi
fi
if ((project_ok)); then
  if gc billing projects describe "$project" --format='value(billingEnabled,billingAccountName)'; then
    read -r enabled linked <<<"$out"
    if [[ $enabled != True ]]; then
      fail "billing is not enabled on $project" "gcloud billing projects link $project --billing-account=<BILLING_ACCOUNT_ID>"
    elif [[ -n $billing && $linked != "billingAccounts/$billing" ]]; then
      fail "$project is linked to another billing account than BILLING_ACCOUNT_ID" \
        "fix BILLING_ACCOUNT_ID in .env, or link the project to it"
    else
      pass "billing enabled on $project${billing:+, linked to BILLING_ACCOUNT_ID}"
      billing_ok=1
    fi
  else
    fail "could not read the billing link of $project" "${err:-}"
  fi
  if [[ -n $billing ]]; then
    if gc billing accounts describe "$billing" --format='value(open,currencyCode)'; then
      read -r open currency <<<"$out"
      if [[ $open == True ]]; then
        pass "billing account is open"
      else
        fail "billing account is closed (trial ended or credit spent?)" "Console > Billing"
      fi
      if [[ -z $currency ]]; then
        warn "billing account currency not reported: budget.tf sets the budget in USD and the apply fails on another currency" \
          "check Console > Billing > Account management"
      elif [[ $currency == USD ]]; then
        pass "billing account currency USD (budget.tf uses USD)"
      else
        fail "billing account currency is $currency: budget.tf sets USD, so the budget apply fails" \
          "the currency can't be changed; set currency_code in infra/envs/lab/budget.tf (a PR) first"
      fi
    else
      fail "could not read the billing account" "${err:-}" "it needs Billing Account Viewer or more on the account"
    fi
  fi
  member="user:$(printf '%s' "$account" | tr '[:upper:]' '[:lower:]')"
  if gc projects get-iam-policy "$project" --flatten='bindings[].members' \
    --filter='bindings.role=roles/owner' --format='value(bindings.members)'; then
    if printf '%s\n' "$out" | tr '[:upper:]' '[:lower:]' | grep -qxF "$member"; then
      pass "you are Owner of $project"
    else
      warn "you are not a direct Owner of $project: bootstrap creates identities and grants roles" \
        "infra/README.md, apply order step 1"
    fi
  else
    warn "could not read the IAM policy of $project" "${err:-}"
  fi
  if [[ -n $billing && $phase != after-up ]]; then
    if gc billing accounts get-iam-policy "$billing" --flatten='bindings[].members' \
      --filter='bindings.role=roles/billing.admin' --format='value(bindings.members)'; then
      if printf '%s\n' "$out" | tr '[:upper:]' '[:lower:]' | grep -qxF "$member"; then
        pass "you are Billing Account Administrator (bootstrap grants roles on the account)"
      else
        warn "you are not a direct Billing Account Administrator: bootstrap's grants on the billing account may fail"
      fi
    else
      warn "could not read the billing account's IAM policy" "${err:-}"
    fi
  fi
  if gc services list --enabled --project="$project" --format='value(config.name)'; then
    enabled_apis=$out
    if printf '%s\n' "$enabled_apis" | grep -qx compute.googleapis.com; then compute_on=1; fi
  else
    warn "could not list the enabled APIs" "${err:-}"
  fi
fi

section "APIs"
if ((!project_ok)); then
  skip "API checks: no readable project"
elif [[ $phase == before-bootstrap ]]; then
  # Terraform reads the project before it can enable anything, with the project as quota project.
  missing=''
  for api in serviceusage.googleapis.com cloudresourcemanager.googleapis.com; do
    printf '%s\n' "$enabled_apis" | grep -qx "$api" || missing+=" $api"
  done
  if [[ -n $missing ]]; then
    warn "not enabled yet:$missing (bootstrap reads the project before it enables APIs; unverified whether that stops it)" \
      "safe to do first: gcloud services enable$missing --project=$project"
  else
    pass "serviceusage and cloudresourcemanager are on, so bootstrap can start"
  fi
else
  missing=''
  while IFS= read -r api; do
    printf '%s\n' "$enabled_apis" | grep -qx "$api" || missing+=" $api"
  done < <(grep -o '"[a-z0-9.-]*\.googleapis\.com"' "$bootstrap_dir/apis.tf" | tr -d '"')
  if [[ -n $missing ]]; then
    fail "APIs from infra/bootstrap/apis.tf not enabled:$missing" "make bootstrap"
  else
    pass "every API in infra/bootstrap/apis.tf is enabled"
  fi
fi

# --- State (before-up, after-up) --------------------------------------------------------------

if [[ $phase != before-bootstrap ]]; then
  section "Bootstrap results"
  if ((project_ok)) && [[ -f $bootstrap_dir/registry.tf ]]; then
    if gc artifacts repositories describe mamori --location="$region" --project="$project" --format='value(name)'; then
      pass "image repository mamori exists (bootstrap; it outlives make down)"
    else
      fail "image repository mamori not found" "${err:-}" "make bootstrap creates it"
    fi
  fi
  if ((!project_ok)); then
    skip "state checks: no readable project"
  elif gc storage buckets describe "gs://$bucket" --format='value(name)'; then
    pass "state bucket gs://$bucket exists"
    if gc storage ls "gs://$bucket/bootstrap/terraform.tfstate"; then
      pass "a copy of the bootstrap state is in gs://$bucket/bootstrap/"
    else
      warn "no copy of the bootstrap state in the bucket; the local file is its only record" \
        "gcloud storage cp infra/bootstrap/terraform.tfstate gs://$bucket/bootstrap/"
    fi
  else
    fail "state bucket gs://$bucket not found" "${err:-}" "make bootstrap creates it"
  fi
  if [[ -f $bootstrap_dir/terraform.tfstate ]]; then
    pass "infra/bootstrap/terraform.tfstate is here (keep it safe)"
  else
    warn "no infra/bootstrap/terraform.tfstate in this checkout" \
      "if bootstrap ran elsewhere, copy its state here before the next make bootstrap"
  fi
fi

# --- Quotas ------------------------------------------------------------------------------------

section "Quotas (region $region and global)"
if ((!project_ok)); then
  skip "quota checks: no readable project"
elif ((!compute_on)); then
  skip "quota checks: the Compute Engine API is off until make bootstrap enables it" \
    "run again with --phase before-up after make bootstrap"
else
  router_exists=0 ip_exists=0
  if gc compute instances list --project="$project" --filter="name ~ ^gke-$cluster-" --format='value(name)'; then
    lab_nodes=$(printf '%s\n' "$out" | grep -c . || true)
  fi
  if gc compute routers list --project="$project" --filter="name=$router_name" --format='value(name)' && [[ -n $out ]]; then
    router_exists=1
  fi
  if gc compute addresses list --project="$project" --global --filter="name=$frontend_ip_name" --format='value(name)' && [[ -n $out ]]; then
    ip_exists=1
  fi
  cpu_need=$((plan_vcpus - lab_nodes * node_vcpus))
  ((cpu_need >= 0)) || cpu_need=0
  if ((lab_nodes)); then
    info "$lab_nodes lab node(s) already running, so the plan needs $cpu_need more vCPU (of $plan_vcpus at a surge upgrade)"
  fi
  if gc compute regions describe "$region" --project="$project" --flatten=quotas \
    --format='csv[no-heading](quotas.metric,quotas.limit,quotas.usage)'; then
    regional=$out
    if lu=$(quota "$regional" PREEMPTIBLE_CPUS); then
      read -r plimit pusage <<<"$lu"
      record "$region PREEMPTIBLE_CPUS: limit $(num "$plimit"), in use $(num "$pusage")"
    fi
    sev=FAIL
    # Spot VMs use preemptible quota when the project has some; only CPUS counts otherwise.
    if lu=$(quota "$regional" PREEMPTIBLE_CPUS) && read -r plimit pusage <<<"$lu" &&
      awk -v l="$plimit" -v u="$pusage" -v n="$cpu_need" 'BEGIN { exit !(l - u >= n && n > 0) }'; then
      sev=WARN
    fi
    check_quota "$region" "$regional" CPUS "$cpu_need" "$sev"
    check_quota "$region" "$regional" IN_USE_ADDRESSES "$((1 - router_exists))" FAIL
  else
    fail "could not read the quotas of $region" "${err:-}"
  fi
  if gc compute project-info describe --project="$project" --flatten=quotas \
    --format='csv[no-heading](quotas.metric,quotas.limit,quotas.usage)'; then
    global=$out
    check_quota global "$global" CPUS_ALL_REGIONS "$cpu_need" FAIL
    check_quota global "$global" STATIC_ADDRESSES "$((1 - ip_exists))" WARN
    check_quota global "$global" IN_USE_ADDRESSES "$((1 - ip_exists))" WARN
  else
    fail "could not read the global quotas" "${err:-}"
  fi
fi

# --- GitHub ------------------------------------------------------------------------------------

section "GitHub ($repo; names only)"
if ((!gh_ok)); then
  skip "GitHub checks: gh missing or not logged in"
else
  want_id=$(tf_default "$bootstrap_dir/variables.tf" github_repository_id)
  want_sub=${TF_VAR_github_subject_prefix:-$(tf_default "$bootstrap_dir/variables.tf" github_subject_prefix)}
  if ghapi "repos/$repo" --jq '"\(.id) \(.permissions.admin // false)"'; then
    read -r repo_id is_admin <<<"$out"
    if [[ -n $want_id && $repo_id != "$want_id" ]]; then
      fail "repository id is $repo_id, bootstrap trusts $want_id (github_repository_id)" "infra/bootstrap/variables.tf"
    else
      pass "repository id $repo_id matches bootstrap"
    fi
    if [[ $is_admin == true ]]; then
      pass "you are admin on $repo (make bootstrap sets environment lab with gh api)"
    elif [[ $phase == before-bootstrap ]]; then
      fail "you are not admin on $repo: make bootstrap can't create environment lab" "gh auth status; log in as the owner"
    else
      warn "you are not admin on $repo: environment lab and the secrets can't be checked or changed"
    fi
  else
    fail "can't read $repo" "${err:-}"
  fi
  if ghapi "repos/$repo/actions/oidc/customization/sub" --jq .sub_claim_prefix; then
    if [[ -n $want_sub && $out != "$want_sub" ]]; then
      fail "GitHub's OIDC subject prefix is '$out', bootstrap trusts '$want_sub': no CI job could get Google credentials" \
        "set github_subject_prefix in infra/bootstrap/variables.tf to GitHub's value, then make bootstrap"
    else
      pass "OIDC subject prefix matches bootstrap ($out)"
    fi
  else
    warn "could not read the OIDC subject customization" "${err:-}"
  fi

  if [[ $phase != before-bootstrap ]]; then
    # Same names as scripts/print-repo-vars.sh prints.
    if ghapi "repos/$repo/actions/variables" --paginate --jq '.variables[].name'; then
      gh_vars=$out gh_vars_ok=1
      missing=''
      for v in GCP_PROJECT_ID GCP_WIF_PROVIDER GCP_TF_PLAN_SA GCP_TF_APPLY_SA TF_STATE_BUCKET; do
        printf '%s\n' "$out" | grep -qx "$v" || missing+=" $v"
      done
      if [[ -n $missing ]]; then
        todo "repository variables missing:$missing" "paste the gh commands make bootstrap printed (scripts/print-repo-vars.sh prints them again)"
      else
        pass "repository variables for CI are set"
      fi
    else
      warn "could not list the repository variables" "${err:-}"
    fi
    # GCP_BILLING_ACCOUNT_ID, ALERT_EMAIL when it is set locally (CI plans would otherwise remove
    # the e-mail channel), plus one secret per lab input without a default.
    want_secrets=GCP_BILLING_ACCOUNT_ID
    if [[ -n $alert_email ]]; then want_secrets+=" ALERT_EMAIL"; fi
    while IFS= read -r name; do
      case $name in project_id | billing_account_id | zone | frontend_hostname | '') continue ;; esac
      want_secrets+=" $(printf '%s' "$name" | tr '[:lower:]' '[:upper:]')"
    done < <(required_tf_vars "$lab_dir")
    if ghapi "repos/$repo/actions/secrets" --paginate --jq '.secrets[].name'; then
      missing=''
      for v in $want_secrets; do
        printf '%s\n' "$out" | grep -qx "$v" || missing+=" $v"
      done
      if [[ -n $missing ]]; then
        todo "repository secrets missing:$missing" "gh secret set <NAME> --repo $repo   (it asks for the value)"
      else
        pass "repository secrets set: $want_secrets"
      fi
    else
      warn "could not list the repository secrets (needs admin on the repository)" "${err:-}"
    fi
    # tf-apply trusts every job in environment lab; its rules are what keep branches from applying.
    if ghapi "repos/$repo/environments/lab" --jq '[
        ([.protection_rules[]? | select(.type == "required_reviewers")] | length),
        ([.protection_rules[]? | select(.type == "required_reviewers") | .prevent_self_review == true] | any),
        (.deployment_branch_policy.protected_branches // "none"),
        (.deployment_branch_policy.custom_branch_policies // "none")
      ] | map(tostring) | join(" ")'; then
      read -r reviewers self_review protected custom <<<"$out"
      if ((reviewers > 0)); then
        pass "environment lab has a required reviewer"
      else
        fail "environment lab has no required reviewer: any job that names it can apply" \
          "make bootstrap sets it (scripts/github-lab-env.sh), or Settings > Environments > lab"
      fi
      if [[ $self_review == true ]]; then
        warn "environment lab prevents self-review: as the only reviewer you can't approve your own applies"
      fi
      if [[ $custom == true ]]; then
        if ghapi "repos/$repo/environments/lab/deployment-branch-policies" --jq '.branch_policies[].name'; then
          if [[ $out == main ]]; then
            pass "environment lab deploys from main only"
          else
            warn "environment lab allows these branches: $(printf '%s' "$out" | tr '\n' ' ')" "allow main only"
          fi
        else
          warn "could not read the branch rules of environment lab" "${err:-}"
        fi
      elif [[ $protected == true ]]; then
        warn "environment lab allows every protected branch" "Deployment branches: Selected branches, main only"
      else
        fail "environment lab allows every branch" "make bootstrap sets main only (scripts/github-lab-env.sh)"
      fi
    elif [[ $err == *404* ]]; then
      fail "environment lab doesn't exist: GitHub would create it unprotected the first time a job names it" \
        "make bootstrap creates it (scripts/github-lab-env.sh; runbook step 5)"
    else
      warn "could not read environment lab" "${err:-}"
    fi
  fi
fi

# --- The running lab (after-up) ---------------------------------------------------------------

if [[ $phase == after-up ]]; then
  section "Cluster and workloads"
  if ((!project_ok)); then
    skip "cluster checks: no readable project"
  else
    if gc container clusters describe "$cluster" --zone="$zone" --project="$project" \
      --format='value(status,currentMasterVersion)'; then
      read -r cstatus cversion <<<"$out"
      if [[ $cstatus == RUNNING ]]; then
        pass "cluster $cluster is RUNNING ($cversion)"
      else
        fail "cluster $cluster is $cstatus" "gcloud container operations list --zone=$zone"
      fi
    else
      fail "cluster $cluster not found" "${err:-}" "make up"
    fi
  fi
  context=gke_${project}_${zone}_$cluster
  if ((kubectl_ok)) && kc config current-context && [[ $out == "$context" ]]; then
    pass "kubectl points at $context"
    if kc get nodes -l "cloud.google.com/gke-nodepool=$node_pool" --no-headers; then
      total=$(printf '%s\n' "$out" | grep -c . || true)
      ready=$(printf '%s\n' "$out" | awk '$2 == "Ready"' | grep -c . || true)
      if ((total == 0)); then
        warn "$node_pool has no nodes: the spend guard (or a test) scaled it to zero" \
          "docs/runbooks/spend-guard.md, then make resume"
      elif ((ready >= 3)); then
        pass "$ready of $total nodes in $node_pool are Ready"
      else
        fail "only $ready of $total nodes in $node_pool are Ready" "kubectl get nodes; kubectl describe node <name>"
      fi
    else
      fail "kubectl get nodes failed" "${err:-}"
    fi
    if kc -n argocd get applications.argoproj.io \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.sync.status}{" "}{.status.health.status}{"\n"}{end}'; then
      apps=$out bad_core='' bad_other=''
      while read -r name sync health; do
        [[ -n $name ]] || continue
        [[ $health == Healthy ]] && continue
        case $name in
          istio-base | istiod | istio-ingressgateway | online-boutique | edge) bad_core+=" $name($sync/$health)" ;;
          *) bad_other+=" $name($sync/$health)" ;;
        esac
      done <<<"$apps"
      if [[ -z $apps ]]; then
        fail "Argo CD has no applications" "kubectl apply -f deploy/bootstrap/root-app.yaml (make up does it)"
      elif [[ -n $bad_core ]]; then
        fail "Argo CD apps the shop needs aren't Healthy:$bad_core" "kubectl -n argocd get applications; runbook step 10"
      else
        pass "Argo CD: Istio, online-boutique and edge are Healthy"
      fi
      if [[ -n $bad_other ]]; then
        warn "other Argo CD apps not Healthy:$bad_other" \
          "kubectl -n argocd get applications.argoproj.io <name> -o yaml; apps in later sync waves wait for these"
      fi
    else
      fail "can't list Argo CD applications" "${err:-}"
    fi
    if kc -n istio-ingress get gateways.gateway.networking.k8s.io mamori-frontend \
      -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}'; then
      if [[ $out == True ]]; then
        pass "Gateway mamori-frontend is Programmed (the load balancer exists)"
      else
        fail "Gateway mamori-frontend is not Programmed" \
          "kubectl -n istio-ingress describe gateways.gateway.networking.k8s.io mamori-frontend"
      fi
    elif [[ $err == *NotFound* ]]; then
      edge_off=1
      warn "Gateway mamori-frontend doesn't exist: the edge is off (make edge-off), so the shop isn't public" \
        "make edge-on brings it back"
    else
      fail "can't read Gateway mamori-frontend" "${err:-}"
    fi
    # Our own images: the prober runs once its image is published (runbook step 15); the executor
    # and the agent stay at 0 replicas until M5.
    for spec in prober/prober mamori-agent/executor mamori-agent/agent; do
      ns=${spec%/*} name=${spec#*/}
      if ! kc -n "$ns" get deployment "$name" -o jsonpath='{.spec.replicas} {.status.readyReplicas}'; then
        if [[ $name == prober ]]; then
          warn "no Deployment prober/prober yet" "Argo CD syncs it in wave 5; kubectl -n argocd get applications"
        fi
        continue
      fi
      read -r want_replicas ready_replicas <<<"$out"
      ready_replicas=${ready_replicas:-0}
      if [[ $name != prober ]]; then
        if ((want_replicas > 0)); then
          warn "$name runs $want_replicas replica(s); it should stay at 0 until M5" "docs/agent.md, docs/executor.md"
        fi
      elif ((want_replicas == 0)); then
        warn "prober has 0 replicas: no journey probes and no SLO data yet" "runbook step 15: publish the images, set-registry, then replicas 1"
      elif ((ready_replicas >= want_replicas)); then
        pass "prober is running ($ready_replicas/$want_replicas Ready)"
      else
        fail "prober wants $want_replicas replica(s) but $ready_replicas are Ready" \
          "kubectl -n prober describe pods (ImagePullBackOff: the image or set-registry is missing)"
      fi
    done
  elif ((kubectl_ok)); then
    warn "kubectl's context is '${out:-<none>}', not $context: the in-cluster checks are skipped" \
      "gcloud container clusters get-credentials $cluster --zone $zone --project $project"
  else
    skip "in-cluster checks: kubectl not installed"
  fi

  section "Edge: IP, DNS, certificate, shop"
  ip=''
  if ((project_ok)) && gc compute addresses describe "$frontend_ip_name" --global --project="$project" \
    --format='value(address,status)'; then
    read -r ip ipstatus <<<"$out"
    if [[ $ipstatus == IN_USE ]]; then
      pass "static IP $ip is IN_USE by the load balancer"
    else
      warn "static IP $ip is $ipstatus: no load balancer uses it (edge off, or the Gateway isn't ready)"
    fi
  elif ((project_ok)); then
    fail "static IP $frontend_ip_name not found" "${err:-}"
  fi
  if ! have dig && ! have host; then
    skip "DNS checks: no dig or host"
  elif [[ -n $ip ]]; then
    answers=$(lookup A "$hostname" | grep -E '^[0-9]+(\.[0-9]+){3}$' || true)
    if [[ -z $answers ]]; then
      fail "$hostname has no A record yet" "add: $hostname A $ip, DNS only (runbook step 11)"
    elif [[ $answers == "$ip" ]]; then
      pass "$hostname resolves to $ip"
    else
      fail "$hostname resolves to $(printf '%s' "$answers" | tr '\n' ' ') instead of $ip" \
        "update the A record; if Cloudflare proxies it (orange cloud), set it to DNS only"
    fi
  fi
  cert_state=''
  if ((project_ok)) && gc certificate-manager dns-authorizations describe "$dnsauth_name" --location=global \
    --project="$project" --format='value(dnsResourceRecord.name,dnsResourceRecord.data)'; then
    read -r cname_name cname_data <<<"$out"
    cname_name=${cname_name%.} cname_data=$(printf '%s' "${cname_data%.}" | tr '[:upper:]' '[:lower:]')
    if have dig || have host; then
      got=$(lookup CNAME "$cname_name" | head -n 1)
      if [[ $got == "$cname_data" ]]; then
        pass "$cname_name is a CNAME to the DNS authorization"
      elif [[ -z $got ]]; then
        fail "$cname_name has no CNAME yet" "add: $cname_name CNAME $cname_data, DNS only"
      else
        fail "$cname_name points to $got, not $cname_data" "fix the CNAME (it can change after make down / make up)"
      fi
    fi
  elif ((project_ok)); then
    fail "DNS authorization $dnsauth_name not found" "${err:-}"
  fi
  if ((project_ok)) && gc certificate-manager certificates describe "$cert_name" --location=global \
    --project="$project" --format='value(managed.state)'; then
    cert_state=$out
    case $cert_state in
      ACTIVE) pass "certificate $cert_name is ACTIVE" ;;
      PROVISIONING) warn "certificate $cert_name is PROVISIONING: waits for the CNAME, then usually minutes (unverified)" ;;
      *) fail "certificate $cert_name is ${cert_state:-in an unknown state}" \
        "gcloud certificate-manager certificates describe $cert_name --location=global" ;;
    esac
  elif ((project_ok)); then
    fail "certificate $cert_name not found" "${err:-}"
  fi
  if ! have curl; then
    skip "HTTPS check: no curl"
  elif ((edge_off)); then
    skip "HTTPS check: the edge is off"
  elif [[ $cert_state != ACTIVE ]]; then
    skip "HTTPS check: waits for the certificate"
  else
    banner=$(sed -n 's/^ *value: "\(Demo shop[^"]*\)".*/\1/p' "$root/deploy/online-boutique/kustomization.yaml" 2>/dev/null | head -n 1 || true)
    banner=${banner:-Demo shop, no real payments}
    code=$(curl -sS --max-time 20 -o "$bodyfile" -w '%{http_code}' "https://$hostname/" 2>"$errfile" || true)
    if [[ $code == 200 ]] && grep -qF "$banner" "$bodyfile"; then
      pass "https://$hostname/ answers 200 with the banner"
    elif [[ $code == 200 ]]; then
      fail "https://$hostname/ answers 200 but without the banner '$banner'" "kubectl -n boutique get deploy frontend -o yaml | grep -A1 FRONTEND_MESSAGE"
    else
      fail "https://$hostname/ answered ${code:-nothing} $(head -n 1 "$errfile")" "runbook step 13"
    fi
    redirect=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code} %{redirect_url}' "http://$hostname/" 2>/dev/null || true)
    if [[ $redirect == "301 https://$hostname/"* ]]; then
      pass "http:// redirects to https://"
    else
      warn "http://$hostname/ answered '${redirect:-nothing}', expected a 301 to https"
    fi
  fi

  section "Second bootstrap, spend guard, budget, paging"
  if [[ -f $bootstrap_dir/observability_identity.tf ]]; then
    flag=0
    if [[ ${TF_VAR_gke_workload_pool_exists:-} == true ]] ||
      grep -Eqs '^[[:space:]]*gke_workload_pool_exists[[:space:]]*=[[:space:]]*true' "$bootstrap_dir/terraform.tfvars"; then
      flag=1
    fi
    granted=0
    if ((project_ok)) && gc projects get-iam-policy "$project" --flatten='bindings[].members' \
      --filter='bindings.role=roles/cloudtrace.agent' --format='value(bindings.members)' &&
      printf '%s\n' "$out" | grep -q 'svc\.id\.goog/subject/ns/observability/sa/otel-collector$'; then
      granted=1
    fi
    if ((granted && flag)); then
      pass "Workload Identity grants applied (second bootstrap done)"
    elif ((granted)); then
      warn "Workload Identity grants exist, but gke_workload_pool_exists = true isn't in infra/bootstrap/terraform.tfvars" \
        "a later make bootstrap without it removes them; add the line"
    elif ((flag)); then
      fail "gke_workload_pool_exists is true, but the trace collector's grant is missing" "make bootstrap"
    else
      warn "second bootstrap not done: the trace collector (and later the agent) has no Google access yet" \
        "echo 'gke_workload_pool_exists = true' >> infra/bootstrap/terraform.tfvars && make bootstrap"
    fi
  fi
  if ((project_ok)) && gc functions describe spend-guard --region="$region" --project="$project" \
    --format='value(state,serviceConfig.environmentVariables.DRY_RUN,serviceConfig.environmentVariables.THRESHOLD)'; then
    read -r fstate dry threshold <<<"$out"
    if [[ $fstate == ACTIVE ]]; then
      pass "spend guard is ACTIVE (DRY_RUN=${dry:-?}, THRESHOLD=${threshold:-?})"
    else
      fail "spend guard is ${fstate:-in an unknown state}" "gcloud functions describe spend-guard --region=$region"
    fi
  elif ((project_ok)); then
    fail "spend guard function not found" "${err:-}"
  fi
  if ((project_ok && billing_ok)) && [[ -n $billing ]]; then
    if gc billing budgets list --billing-account="$billing" --billing-project="$project" --format='value(displayName)'; then
      if printf '%s\n' "$out" | grep -qx mamori-lab; then
        pass "budget mamori-lab exists"
      else
        fail "budget mamori-lab not found on the billing account" "make up creates it (infra/envs/lab/budget.tf)"
      fi
    else
      warn "could not list budgets" "${err:-}"
    fi
  fi
  if grep -qs 'google_monitoring_notification_channel" "pagerduty"' "$lab_dir"/*.tf; then
    info "PagerDuty: the channel starts with a placeholder key, so nothing pages until you set the real one by hand" \
      "optional for #15; docs/oncall.md, 'Set up, once', step 7 (again after every make down / make up)"
  fi
  section "Images and the prober"
  # The version deploy/ runs: an image: line, or a kustomization images: entry (name, then newTag).
  prober_tag=$(sed -n 's|.*docker\.pkg\.dev/[^/]*/mamori/prober:\([0-9][0-9.]*\).*|\1|p' "$root"/deploy/prober/*.yaml 2>/dev/null | head -n 1 || true)
  if [[ -z $prober_tag ]]; then
    prober_tag=$(awk '/name:.*mamori\/prober/ { f = 1 } f && /newTag:/ { print $2; exit }' "$root/deploy/prober/kustomization.yaml" 2>/dev/null | tr -d '"' || true)
  fi
  if ((!project_ok)); then
    skip "image checks: no readable project"
  elif [[ -z $prober_tag ]]; then
    skip "image checks: no version tag for the prober in deploy/prober"
  elif gc artifacts docker images list "$region-docker.pkg.dev/$project/mamori/prober" --include-tags \
    --format='value(tags)'; then
    if printf '%s\n' "$out" | awk -F'[,;]' '{ for (i = 1; i <= NF; i++) print $i }' | grep -qx "$prober_tag"; then
      pass "image prober:$prober_tag is in the registry"
    else
      warn "image prober:$prober_tag is not in the registry yet" \
        "runbook step 15: publish once with images.yml (docs/images.md, 'First time: the order')"
    fi
  else
    warn "could not list the prober's images" "${err:-}"
  fi
  placeholders=$(registry_placeholders)
  if [[ -n $placeholders ]]; then
    warn "deploy/ still names the registry project PROJECT_ID" \
      "runbook step 15: scripts/set-registry.sh --apply ${project:-<PROJECT_ID>}, then merge (Argo CD reads main)" \
      "files: $(printf '%s' "$placeholders" | tr '\n' ' ')"
  else
    pass "deploy/ names the real registry project"
  fi
fi

# --- SLOs --------------------------------------------------------------------------------------

# Generated SLOs, services and alerts exist only while slo_enabled is true; Cloud Monitoring rejects
# filters on metric types that don't exist yet, so they wait until the prober has run.
section "SLOs (slo_enabled)"
slo_on=0
if [[ $slo_setting == true ]]; then slo_on=1; fi
if [[ $phase != after-up ]]; then
  if ((slo_on && lab_nodes == 0)); then
    warn "SLO_ENABLED=true before the lab has run: Cloud Monitoring rejects SLOs on metrics that don't exist yet" \
      "leave it out of .env for the first make up; runbook step 18 turns it on"
  elif ((slo_on)); then
    pass "SLO_ENABLED=true"
  else
    info "SLOs off (SLO_ENABLED unset): right for the first make up; runbook step 18 turns them on"
  fi
else
  slos=-1
  if ! have terraform || [[ ! -d $lab_dir/.terraform ]]; then
    skip "SLO state: infra/envs/lab isn't initialised here (make up or make plan does it)"
  elif out=$(terraform -chdir="$lab_dir" state list 2>"$errfile"); then
    slos=$(printf '%s\n' "$out" | grep -c '^google_monitoring_slo\.' || true)
  else
    warn "could not read the lab's Terraform state" "$(head -n 1 "$errfile")"
  fi
  if ((slos > 0 && slo_on)); then
    pass "SLOs are on: $slos SLOs in the lab"
  elif ((slos > 0)); then
    warn "the lab has $slos SLOs, but SLO_ENABLED isn't true here: the next make up deletes them" \
      "put SLO_ENABLED=true in .env"
  elif ((slos == 0 && slo_on)); then
    warn "SLO_ENABLED=true, but the lab has no SLOs yet" "make up (runbook step 18)"
  elif ((slos == 0)); then
    warn "SLOs off: no SLO, burn-rate or no-data alerts yet" "runbook step 18, once the prober has run about 10 minutes"
  fi
  if ((slos > 0 || slo_on)) && ((gh_vars_ok)); then
    if printf '%s\n' "$gh_vars" | grep -qix -e SLO_ENABLED -e TF_VAR_SLO_ENABLED; then
      pass "repository variable SLO_ENABLED exists, so CI applies keep the SLOs (its value isn't read)"
    else
      todo "repository variable SLO_ENABLED is missing: a CI apply would delete the SLOs" \
        "gh variable set SLO_ENABLED --repo $repo --body true"
    fi
  fi
fi

# --- Summary -----------------------------------------------------------------------------------

if [[ -n $records ]]; then
  printf '\nQuota to write down in #15 (%s):\n%s' "$(date -u +%Y-%m-%dT%H:%MZ)" "$records"
fi
printf '\nSummary (%s): %d PASS, %d WARN, %d FAIL\n' "$phase" "$npass" "$nwarn" "$nfail"
if ((nfail)); then
  echo "Fix the FAIL lines, then run this again. docs/runbooks/first-run.md says what each step needs."
  exit 1
fi
case $phase in
  before-bootstrap) echo "Ready for: make bootstrap" ;;
  before-up) echo "Ready for: make up" ;;
  after-up) echo "The lab is up. Read the WARN lines; some are expected on the first day (runbook)." ;;
esac
