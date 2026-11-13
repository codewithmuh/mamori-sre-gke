#!/usr/bin/env bash
# Waits until the GKE Gateway controller has deleted the load balancer it built for the shop.
# `make down` runs this after deleting the Gateway and before `terraform destroy`: if the cluster
# goes first, the controller never cleans up, and forwarding rules, proxies, URL maps, backend
# services and NEGs stay behind. They are billable, and they block deleting the Cloud Armor policy,
# the certificate map, the static IP and the VPC.
#
# Usage: scripts/wait-for-lb-gone.sh [FRONTEND_IP]
#   FRONTEND_IP      the Gateway's static IP (Terraform output frontend_ip); also waits for any
#                    forwarding rule on it
#   PROJECT_ID       required
#   TIMEOUT_SECONDS  default 900
# Read-only: it only runs `gcloud ... list`.
set -euo pipefail

project=${PROJECT_ID:?PROJECT_ID is not set}
ip=${1:-}
timeout=${TIMEOUT_SECONDS:-900}
interval=15

# Name prefixes the GKE Gateway controller (gkegw1-) and the NEG controller (k8s1-) use. The
# project holds only the lab, so anything with these prefixes belongs to the shop's Gateway.
gateway_prefix='gkegw1-'
neg_prefix='k8s1-'

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

remaining() {
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

deadline=$((SECONDS + timeout))
while :; do
  if ! left=$(remaining); then
    echo "error: could not list load balancer resources in ${project}" >&2
    exit 2
  fi
  if [[ -z $left ]]; then
    echo "The shop's load balancer is gone."
    exit 0
  fi
  if ((SECONDS >= deadline)); then
    echo "error: after ${timeout}s these load balancer resources still exist:" >&2
    prefix '  ' <<<"$left" >&2
    echo "Check that no Gateway is left (kubectl get gateways.gateway.networking.k8s.io -A), that the" >&2
    echo "istio-ingress Service is gone, and that the cluster is healthy; then rerun 'make down'." >&2
    exit 1
  fi
  echo "Waiting for the GKE Gateway controller to delete $(wc -l <<<"$left" | tr -d ' ') resource(s):"
  prefix '  ' <<<"$left"
  sleep "$interval"
done
