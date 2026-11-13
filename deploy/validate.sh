#!/usr/bin/env bash
# Render everything Argo CD would apply from deploy/ and check it offline: no cluster needed.
#   1. helm template: the Argo CD bootstrap chart, and each Helm Application in deploy/apps
#      (chart, version, values file and namespace are read from the Application itself).
#   2. kubectl kustomize: every kustomization under deploy/ (remote bases need network).
#   3. kubeconform -strict against Kubernetes $K8S_VERSION, with CRD schemas for Argo CD,
#      Gateway API, Istio and the GKE policies from the datreeio CRDs-catalog.
#   4. No Service of type LoadBalancer anywhere in the output.
# Needs bash, awk, helm, kubectl, and kubeconform on PATH or docker (pinned image below).
# Usage: deploy/validate.sh   (RENDER_DIR=dir keeps the rendered output)
set -euo pipefail

cd "$(dirname "$0")/.."

# Kubernetes minor of GKE's REGULAR channel default (1.36.4-gke.2377000 on 2026-11-13).
K8S_VERSION="${K8S_VERSION:-1.36.4}"
# Keep in step with the Makefile, which installs Argo CD with this chart version.
ARGOCD_CHART_VERSION="${ARGOCD_CHART_VERSION:-10.11.6}"
KUBECONFORM_IMAGE="ghcr.io/yannh/kubeconform:v0.8.0@sha256:faffaf43f95aa6425306e1ab8d6fcad72acb9049158f38e574c085ea1ec0f64e"
# Schema repos pinned to commits so a schema change upstream cannot change the result.
K8S_SCHEMAS="https://raw.githubusercontent.com/yannh/kubernetes-json-schema/9e1cd3c3b22e6432cbc5615a5a930f8268f14bf1"
CRD_SCHEMAS="https://raw.githubusercontent.com/datreeio/CRDs-catalog/89c736bdde38b5fecd0f8436ec5f3aa3679a3b86"

out="${RENDER_DIR:-$(mktemp -d)}"
mkdir -p "$out/apps"
[ -n "${RENDER_DIR:-}" ] || trap 'rm -rf "$out"' EXIT

echo "--- helm template: Argo CD bootstrap (argo-cd $ARGOCD_CHART_VERSION)"
helm template argocd argo-cd --repo https://argoproj.github.io/argo-helm \
  --version "$ARGOCD_CHART_VERSION" --namespace argocd --kube-version "$K8S_VERSION" \
  -f deploy/bootstrap/argocd-values.yaml >"$out/argocd.yaml"

# First match of a key in an Application. Our Application files list the chart source first,
# so the first repoURL/targetRevision belong to the chart.
field() { awk -v k="$1:" '$1 == k || ($1 == "-" && $2 == k) { print $NF; exit }' "$2"; }

for app in deploy/apps/*.yaml; do
  cp "$app" "$out/apps/"
  chart="$(field chart "$app")"
  [ -n "$chart" ] || continue
  repo="$(field repoURL "$app")"
  version="$(field targetRevision "$app")"
  release="$(field releaseName "$app")"
  values="$(awk '/\$values\// { sub(/.*\$values\//, ""); print; exit }' "$app")"
  namespace="$(awk '/^  destination:/ { d = 1 } d && $1 == "namespace:" { print $2; exit }' "$app")"
  echo "--- helm template: $chart $version ($app)"
  helm template "$release" "$chart" --repo "$repo" --version "$version" \
    --namespace "$namespace" --kube-version "$K8S_VERSION" -f "$values" >"$out/$release.yaml"
done
cp deploy/bootstrap/root-app.yaml "$out/apps/"

# Every kustomization under deploy/ except kustomize Components, so a new directory is checked
# without editing this script.
find deploy -name kustomization.yaml | sort | while read -r file; do
  grep -q '^kind: Component' "$file" && continue
  dir="$(dirname "$file")"
  echo "--- kubectl kustomize $dir"
  kubectl kustomize "$dir" >"$out/kustomize-$(echo "${dir#deploy/}" | tr / -).yaml"
done

# The third location is only for CustomResourceDefinition objects, whose schema is missing from
# the standalone sets. Any kind with no schema at all fails the run.
kubeconform_args=(
  -strict -summary -output text -kubernetes-version "$K8S_VERSION"
  -schema-location "$K8S_SCHEMAS/{{.NormalizedKubernetesVersion}}-standalone{{.StrictSuffix}}/{{.ResourceKind}}{{.KindSuffix}}.json"
  -schema-location "$CRD_SCHEMAS/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"
  -schema-location "$K8S_SCHEMAS/{{.NormalizedKubernetesVersion}}/{{.ResourceKind}}{{.KindSuffix}}.json"
)
if command -v kubeconform >/dev/null 2>&1; then
  echo "--- kubeconform $(kubeconform -v) from PATH (Kubernetes $K8S_VERSION, strict)"
  kubeconform "${kubeconform_args[@]}" "$out"
else
  echo "--- kubeconform $KUBECONFORM_IMAGE (Kubernetes $K8S_VERSION, strict)"
  docker run --rm -v "$out":/manifests:ro "$KUBECONFORM_IMAGE" "${kubeconform_args[@]}" /manifests
fi

echo "--- no LoadBalancer Services"
if grep -rnE 'type:[[:space:]]*"?LoadBalancer' "$out"; then
  echo "FAIL: a Service of type LoadBalancer would be created; the GKE Gateway is the only public entry" >&2
  exit 1
fi
echo "OK: rendered output in $out"
