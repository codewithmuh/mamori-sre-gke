#!/usr/bin/env bash
# Check every fault manifest offline with kubeconform -strict, the way deploy/validate.sh checks
# what Argo CD applies: Kubernetes schemas and the datreeio CRDs-catalog (Istio) at the commits
# pinned in deploy/validate.sh, plus the Chaos Mesh schemas in chaos/schemas (the catalog has
# none; chaos/schemas/generate.sh makes them from the pinned chart). A kind without a schema
# fails the run.
# The labels, the namespace and the fault file schema are checked by the Go tests in
# tools/gameday (`make gameday-test`).
# Needs bash, and kubeconform on PATH or docker. Usage: chaos/validate.sh
set -euo pipefail

cd "$(dirname "$0")/.."

# One place for the pins: read them from deploy/validate.sh.
pin() { sed -n "s/^$1=\"\${$1:-\(.*\)}\"\$/\1/p; s/^$1=\"\(.*\)\"\$/\1/p" deploy/validate.sh | head -1; }
K8S_VERSION="${K8S_VERSION:-$(pin K8S_VERSION)}"
KUBECONFORM_IMAGE="$(pin KUBECONFORM_IMAGE)"
K8S_SCHEMAS="$(pin K8S_SCHEMAS)"
CRD_SCHEMAS="$(pin CRD_SCHEMAS)"
for v in K8S_VERSION KUBECONFORM_IMAGE K8S_SCHEMAS CRD_SCHEMAS; do
  [ -n "${!v}" ] || { echo "FAIL: could not read $v from deploy/validate.sh" >&2; exit 1; }
done

args=(
  -strict -summary -output text -kubernetes-version "$K8S_VERSION"
  -schema-location "$K8S_SCHEMAS/{{.NormalizedKubernetesVersion}}-standalone{{.StrictSuffix}}/{{.ResourceKind}}{{.KindSuffix}}.json"
  -schema-location "$CRD_SCHEMAS/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"
)

echo "--- kubeconform chaos/faults/manifests (Kubernetes $K8S_VERSION, strict)"
if command -v kubeconform >/dev/null 2>&1; then
  kubeconform "${args[@]}" -schema-location "chaos/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
    chaos/faults/manifests
else
  docker run --rm -v "$PWD/chaos":/chaos:ro "$KUBECONFORM_IMAGE" "${args[@]}" \
    -schema-location "/chaos/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" /chaos/faults/manifests
fi
