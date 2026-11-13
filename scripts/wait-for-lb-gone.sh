#!/usr/bin/env bash
# Waits until the GKE Gateway controller has deleted the load balancer it built for the shop.
# `make down` and `make edge-off` run this after deleting the Gateway: if the cluster goes first,
# the controller never cleans up, and forwarding rules, proxies, URL maps, backend services and
# NEGs stay behind. They are billable, and they block deleting the Cloud Armor policy, the
# certificate map, the static IP and the VPC.
#
# Then it waits for the firewall rules the GKE controllers made in the lab's VPC. Google doesn't
# document when those go, and one left behind makes deleting the VPC fail, so after a short wait
# it deletes the ones still there (the only change this script makes; firewall rules cost nothing).
#
# Usage: scripts/wait-for-lb-gone.sh [FRONTEND_IP]
#   FRONTEND_IP       the Gateway's static IP (Terraform output frontend_ip); also waits for any
#                     forwarding rule on it
#   PROJECT_ID        required
#   NETWORK           the lab's VPC (default mamori-vpc)
#   TIMEOUT_SECONDS   default 900, for the load balancer
#   FIREWALL_SECONDS  default 120, for the firewall rules once the load balancer is gone
set -euo pipefail

project=${PROJECT_ID:?PROJECT_ID is not set}
ip=${1:-}
network=${NETWORK:-mamori-vpc}
timeout=${TIMEOUT_SECONDS:-900}
firewall_timeout=${FIREWALL_SECONDS:-120}
interval=15

# Name prefixes the GKE Gateway controller (gkegw1-) and the NEG controller (k8s1-) use. The
# project holds only the lab, so anything with these prefixes belongs to the shop's Gateway.
gateway_prefix='gkegw1-'
neg_prefix='k8s1-'
# Firewall rules made by the Gateway controller (gkegw1-) and the load balancer and NEG
# controllers (k8s-fw-, k8s2-).
firewall_filter="network~/networks/${network}\$ AND name~^(gkegw1-|k8s-fw-|k8s2-)"

export CLOUDSDK_CORE_DISABLE_PROMPTS=1
errlog=$(mktemp)
trap 'rm -f "$errlog"' EXIT

# prefix TEXT: print stdin with TEXT before every line.
prefix() {
  local line
  while IFS= read -r line; do
    printf '%s%s\n' "$1" "$line"
  done
}

# list KIND FILTER: print "KIND/NAME" per resource; on failure show gcloud's error and return 1.
list() {
  local kind=$1 filter=$2 out
  if ! out=$(gcloud compute "$kind" list --project="$project" --filter="$filter" \
    --format='value(name)' 2>"$errlog"); then
    cat "$errlog" >&2
    return 1
  fi
  if [[ -n $out ]]; then
    prefix "${kind}/" <<<"$out"
  fi
}

load_balancer() {
  {
    if [[ -n $ip ]]; then
      list forwarding-rules "IPAddress=${ip}" || return 1
    fi
    list forwarding-rules "name~^${gateway_prefix}" || return 1
    list target-https-proxies "name~^${gateway_prefix}" || return 1
    list target-http-proxies "name~^${gateway_prefix}" || return 1
    list url-maps "name~^${gateway_prefix}" || return 1
    list backend-services "name~^${gateway_prefix}" || return 1
    list network-endpoint-groups "name~^${neg_prefix}" || return 1
  } | sort -u
}

firewall_rules() {
  list firewall-rules "$firewall_filter"
}

# wait_for WHAT SECONDS FUNCTION: poll FUNCTION until it prints nothing. Returns 1 at the
# deadline (the last list is in $left), exits 2 when a list fails.
wait_for() {
  local what=$1 deadline=$((SECONDS + $2)) fn=$3
  while :; do
    if ! left=$("$fn"); then
      echo "error: could not list ${what} in ${project}" >&2
      exit 2
    fi
    if [[ -z $left ]]; then
      return 0
    fi
    if ((SECONDS >= deadline)); then
      return 1
    fi
    echo "Waiting for the GKE controllers to delete $(wc -l <<<"$left" | tr -d ' ') ${what}:"
    prefix '  ' <<<"$left"
    sleep "$interval"
  done
}

left=""
if ! wait_for "load balancer resource(s)" "$timeout" load_balancer; then
  echo "error: after ${timeout}s these load balancer resources still exist:" >&2
  prefix '  ' <<<"$left" >&2
  echo "Check that no Gateway is left (kubectl get gateways.gateway.networking.k8s.io -A), that the" >&2
  echo "istio-ingress Services are gone, and that the cluster is healthy; then run this again." >&2
  exit 1
fi
echo "The shop's load balancer is gone."

if ! wait_for "firewall rule(s) in ${network}" "$firewall_timeout" firewall_rules; then
  echo "The GKE controllers left these firewall rules in ${network} after ${firewall_timeout}s."
  echo "Deleting them, since the load balancer they served is gone and they would block deleting the VPC:"
  prefix '  ' <<<"$left"
  names=${left//firewall-rules\//}
  # Word splitting is wanted: one argument per rule name (names have no spaces).
  # shellcheck disable=SC2086
  gcloud compute firewall-rules delete $names --project="$project" --quiet
fi
echo "No GKE-made firewall rules are left in ${network}."
