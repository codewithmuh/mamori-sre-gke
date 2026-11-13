#!/usr/bin/env bash
# Deletes what lives in the cluster but costs money outside it, in the order `make down` needs.
# Argo CD goes first, or it would recreate what we delete. Then the Gateway and the Services
# behind it, so the GKE Gateway and NEG controllers delete the load balancer and the NEGs while
# the cluster still runs. scripts/wait-for-lb-gone.sh then waits for that cleanup to finish.
#
# Usage: scripts/delete-shop.sh   (uses the current kubectl context; `make down` sets it)
set -euo pipefail

argocd_running() {
  kubectl -n argocd get pods -l app.kubernetes.io/name=argocd-application-controller \
    -o jsonpath='{.items[*].status.containerStatuses[*].ready}' 2>/dev/null | grep -qw true
}

if kubectl get crd applications.argoproj.io >/dev/null 2>&1; then
  if argocd_running; then
    # Each app carries the resources-finalizer, so deleting root makes Argo CD delete every child
    # app and everything they deployed; kubectl returns once that is done.
    echo "Deleting the Argo CD root app and, through it, every app and its resources..."
    kubectl -n argocd delete applications.argoproj.io root --ignore-not-found --wait --timeout=15m
    kubectl -n argocd delete applications.argoproj.io --all --wait --timeout=10m
  else
    # No controller runs (for example the spend guard scaled the nodes to zero): nothing can
    # recreate what we delete next, and app finalizers would never finish. The apps go with
    # the cluster.
    echo "Argo CD is not running; deleting the Gateway and namespaces directly."
  fi
fi

echo "Deleting Gateway mamori-frontend..."
kubectl delete gateways.gateway.networking.k8s.io --all-namespaces \
  --field-selector metadata.name=mamori-frontend --wait --timeout=10m

echo "Deleting namespaces istio-ingress and boutique (their Services back the load balancer)..."
kubectl delete namespace istio-ingress boutique --ignore-not-found --wait --timeout=10m
