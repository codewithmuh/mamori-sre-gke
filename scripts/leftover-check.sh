#!/usr/bin/env bash
# Lists what is left in the lab project after `make down`.
# Exit 0: nothing unexpected left. Exit 1: something is left (listed). Exit 2: a list call failed,
# so the project could not be checked; a failed check never counts as clean.
#
# Usage: scripts/leftover-check.sh [PROJECT_ID]   (default: $PROJECT_ID)
#   REGION  the lab's region (default asia-northeast1): where the kept resources live, and the
#           region of the Cloud Run functions check
# Read-only: it only runs `gcloud ... list`. Every other list covers all regions and zones (the
# Artifact Registry one only while your gcloud config sets no artifacts/location).
#
# Kept on purpose, so reported as "kept", not as leftovers (from infra/bootstrap):
#   - Artifact Registry repository `mamori`, so images survive `make down` (storage is billed;
#     its cleanup policy keeps it small)
#   - buckets ${PROJECT_ID}-tfstate and ${PROJECT_ID}-spend-guard-src, plus the gcf-v2-* buckets
#     Cloud Run functions creates and never deletes; each holds a few KB (estimate)
# Not checked, because they cost nothing: service accounts, custom roles, the workload identity
# pools, Pub/Sub topics without traffic, VPC networks.
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

# check LABEL FORMAT KEEP GCLOUD_ARGS...: list one resource type. Lines that match the regex KEEP
# (may be empty) are expected; anything else is a leftover.
check() {
  local label=$1 format=$2 keep=$3 out line leftovers="" kept=""
  shift 3
  if ! out=$(gcloud "$@" --project="$project" --format="$format" 2>"$errlog"); then
    failed=1
    printf 'ERROR  %s: could not list\n' "$label"
    prefix '         ' <"$errlog"
    return 0
  fi
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    if [[ -n $keep && $line =~ $keep ]]; then
      kept+="${line}"$'\n'
    else
      leftovers+="${line}"$'\n'
    fi
  done <<<"$out"
  if [[ -n $leftovers ]]; then
    left=1
    printf 'LEFT   %s\n' "$label"
    printf '%s' "$leftovers" | prefix '         '
  else
    printf 'ok     %s\n' "$label"
  fi
  if [[ -n $kept ]]; then
    printf '%s' "$kept" | prefix '         kept: '
  fi
}

kept_repos="/locations/${region}/repositories/mamori$"
kept_buckets="^(${project}-tfstate|${project}-spend-guard-src|gcf-v2-(sources|uploads)-[0-9]+-${region})$"
# Rules in the default network belong to the project, not the lab. Firewall rules cost nothing,
# but a GKE-made rule left in mamori-vpc means the VPC could not be deleted.
kept_firewall=$'\tdefault$'

echo "Leftovers in project ${project}:"
check "GKE clusters" "value(name,location)" "" container clusters list
check "VM instances" "value(name,zone.basename())" "" compute instances list
check "Persistent disks" "value(name,zone.basename())" "" compute disks list
check "Static IP addresses" "value(name,address,region.basename())" "" compute addresses list
check "Forwarding rules" "value(name,IPAddress)" "" compute forwarding-rules list
check "Target HTTPS proxies" "value(name)" "" compute target-https-proxies list
check "Target HTTP proxies" "value(name)" "" compute target-http-proxies list
check "URL maps" "value(name)" "" compute url-maps list
check "Backend services" "value(name)" "" compute backend-services list
check "Network endpoint groups" "value(name,zone.basename())" "" compute network-endpoint-groups list
check "Firewall rules outside the default network" "value(name,network.basename())" "$kept_firewall" \
  compute firewall-rules list
check "Cloud Routers (Cloud NAT)" "value(name,region.basename())" "" compute routers list
check "Cloud Armor policies" "value(name)" "" compute security-policies list
check "Artifact Registry repositories" "value(name)" "$kept_repos" artifacts repositories list
check "Cloud Storage buckets" "value(name)" "$kept_buckets" storage buckets list
check "Cloud Run functions (${region})" "value(name)" "" functions list --regions="$region"

if ((left)); then
  echo "Resources remain in ${project} (marked LEFT above). Rerun 'make down', or delete them by hand." >&2
  exit 1
fi
if ((failed)); then
  echo "Could not check everything (marked ERROR above), so the project is not known to be clean." >&2
  exit 2
fi
echo "Nothing unexpected is left in ${project}."
