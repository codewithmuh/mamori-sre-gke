#!/usr/bin/env bash
# Deletes what lives in the cluster but costs money outside it, in the order `make down` needs.
# Argo CD goes first, or it would recreate what we delete. Then the Gateway and the Services
# behind it, so the GKE Gateway and NEG controllers (they run in Google's control plane, not on
# our nodes) delete the load balancer and the NEGs while the cluster still exists. This script
# does not wait for that: scripts/wait-for-lb-gone.sh does, right after it.
#
# Usage: scripts/delete-shop.sh   (uses the current kubectl context; `make down` sets it)
set -euo pipefail

ready_nodes() {
  kubectl get nodes --no-headers 2>/dev/null | awk '$2 ~ /^Ready/ { n++ } END { print n + 0 }'
}

argocd_running() {
  kubectl -n argocd get pods -l app.kubernetes.io/name=argocd-application-controller \
    -o jsonpath='{.items[*].status.containerStatuses[*].ready}' 2>/dev/null | grep -qw true
}

nodes=$(ready_nodes)
if ((nodes == 0)); then
  # Typically the spend guard scaled the node pool to zero. Nothing runs, so nothing can
  # recreate what we delete, but anything that waits on a workload (app finalizers, namespace
  # deletion behind an unavailable metrics-server APIService) would hang. So: no Argo CD step,
  # and nothing below waits.
  echo "The cluster has no ready nodes (the spend guard may have scaled the pool to 0)."
  echo "Argo CD can't run: deleting the Gateway and the istio-ingress Services directly."
elif kubectl get crd applications.argoproj.io >/dev/null 2>&1 && argocd_running; then
  # Each app carries the resources-finalizer, so deleting root makes Argo CD delete every child
  # app and everything they deployed; kubectl returns once that is done. If that times out, go on:
  # Argo CD never syncs an app that is being deleted, so it can't bring the Gateway back.
  echo "Deleting the Argo CD root app and, through it, every app and its resources..."
  kubectl -n argocd delete applications.argoproj.io root --ignore-not-found --wait --timeout=15m ||
    echo "warning: the root app is not gone yet; going on with the Gateway." >&2
  kubectl -n argocd delete applications.argoproj.io --all --wait --timeout=10m ||
    echo "warning: some apps are not gone yet; going on with the Gateway." >&2
else
  echo "Argo CD is not running: deleting the Gateway and the istio-ingress Services directly."
fi

echo "Deleting Gateway mamori-frontend..."
kubectl delete gateways.gateway.networking.k8s.io --all-namespaces \
  --field-selector metadata.name=mamori-frontend --wait=false

if kubectl get namespace istio-ingress >/dev/null 2>&1; then
  echo "Deleting the Services in istio-ingress (the load balancer's NEGs point at them)..."
  kubectl -n istio-ingress delete services --all --wait=false
fi

# The namespaces go with the cluster anyway; deleting them here only stops anything left from
# serving. --wait=false: a namespace can stay Terminating for a long time.
kubectl delete namespace istio-ingress boutique --ignore-not-found --wait=false
