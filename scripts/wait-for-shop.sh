#!/usr/bin/env bash
# Waits until Argo CD has rolled out the shop and the Gateway has its load balancer.
# `make up` runs this right after applying the root app; Argo CD creates the apps in sync waves,
# so the shop's Deployment only appears after Istio is up.
#
# Usage: scripts/wait-for-shop.sh   (uses the current kubectl context)
#   TIMEOUT_SECONDS  default 1800 for the whole wait
set -euo pipefail

timeout=${TIMEOUT_SECONDS:-1800}
interval=15
deadline=$((SECONDS + timeout))

show_apps() {
  kubectl -n argocd get applications.argoproj.io >&2 || true
}

# wait_until DESCRIPTION COMMAND...: poll until the command succeeds or the deadline passes.
wait_until() {
  local what=$1
  shift
  echo "Waiting for ${what}..."
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      echo "error: ${what} not ready after ${timeout}s. Argo CD apps:" >&2
      show_apps
      exit 1
    fi
    sleep "$interval"
  done
}

seconds_left() {
  local left=$((deadline - SECONDS))
  echo $((left > 60 ? left : 60))
}

gateway_programmed() {
  local status
  status=$(kubectl get gateways.gateway.networking.k8s.io -A \
    --field-selector metadata.name=mamori-frontend \
    -o jsonpath='{.items[0].status.conditions[?(@.type=="Programmed")].status}')
  [[ $status == True ]]
}

wait_until "Argo CD to create the shop (Deployment boutique/frontend)" \
  kubectl -n boutique get deployment/frontend
kubectl -n boutique rollout status deployment/frontend --timeout="$(seconds_left)s"

# Programmed means the GKE Gateway controller has built the load balancer. The certificate can
# still be pending until the DNS records exist.
wait_until "Gateway mamori-frontend to get its load balancer" gateway_programmed

kubectl -n argocd get applications.argoproj.io
