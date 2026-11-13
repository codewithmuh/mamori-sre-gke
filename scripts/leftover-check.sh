#!/usr/bin/env bash
# Lists billable resources left in the lab project after `make down`.
# Exit 0: nothing billable left. Exit 1: something is left (listed). Exit 2: a list call failed,
# so the project could not be checked; a failed check never counts as clean.
#
# Usage: scripts/leftover-check.sh [PROJECT_ID]   (default: $PROJECT_ID)
#   REGION  region for the Cloud Run functions check (default asia-northeast1, the lab's region)
# Read-only: it only runs `gcloud ... list`. Every other list covers all regions and zones (the
# Artifact Registry one only while your gcloud config sets no artifacts/location).
#
# Buckets are not checked: the bootstrap buckets (Terraform state, spend-guard source) stay on
# purpose, and Cloud Run functions leaves a gcf-v2-sources bucket behind; each holds a few KB
# (estimate). Service accounts, the custom role and the workload identity pool cost nothing.
set -euo pipefail

project=${1:-${PROJECT_ID:-}}
region=${REGION:-asia-northeast1}
if [[ -z $project ]]; then
  echo "usage: $0 PROJECT_ID (or set PROJECT_ID)" >&2
  exit 2
fi
if ! command -v gcloud >/dev/null 2>&1; then
  echo "error: gcloud is not installed" >&2
  exit 2
fi
export CLOUDSDK_CORE_DISABLE_PROMPTS=1 # never stop to ask, e.g. to enable an API

errlog=$(mktemp)
trap 'rm -f "$errlog"' EXIT

left=0
failed=0

# prefix TEXT: print stdin with TEXT before every line.
prefix() {
  local line
  while IFS= read -r line; do
    printf '%s%s\n' "$1" "$line"
  done
}

# check LABEL FORMAT GCLOUD_ARGS...: list one resource type and report what is there.
check() {
  local label=$1 format=$2 out
  shift 2
  if ! out=$(gcloud "$@" --project="$project" --format="$format" 2>"$errlog"); then
    failed=1
    printf 'ERROR  %s: could not list\n' "$label"
    prefix '         ' <"$errlog"
    return 0
  fi
  if [[ -n $out ]]; then
    left=1
    printf 'LEFT   %s\n' "$label"
    prefix '         ' <<<"$out"
  else
    printf 'ok     %s\n' "$label"
  fi
}

echo "Billable leftovers in project ${project}:"
check "GKE clusters" "value(name,location)" container clusters list
check "VM instances" "value(name,zone.basename())" compute instances list
check "Persistent disks" "value(name,zone.basename())" compute disks list
check "Static IP addresses" "value(name,address,region.basename())" compute addresses list
check "Forwarding rules" "value(name,IPAddress)" compute forwarding-rules list
check "Target HTTPS proxies" "value(name)" compute target-https-proxies list
check "Target HTTP proxies" "value(name)" compute target-http-proxies list
check "URL maps" "value(name)" compute url-maps list
check "Backend services" "value(name)" compute backend-services list
check "Network endpoint groups" "value(name,zone.basename())" compute network-endpoint-groups list
check "Cloud Routers (Cloud NAT)" "value(name,region.basename())" compute routers list
check "Cloud Armor policies" "value(name)" compute security-policies list
check "Artifact Registry repositories" "value(name)" artifacts repositories list
check "Cloud Run functions (${region})" "value(name)" functions list --regions="$region"

if ((left)); then
  echo "Billable resources remain in ${project} (marked LEFT above). Rerun 'make down', or delete them by hand." >&2
  exit 1
fi
if ((failed)); then
  echo "Could not check everything (marked ERROR above), so the project is not known to be clean." >&2
  exit 2
fi
echo "Nothing billable is left in ${project}."
