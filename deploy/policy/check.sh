#!/usr/bin/env bash
# Check the readiness policies offline with gator (Gatekeeper's CLI); no cluster needed.
#   1. gator verify: the suite in deploy/policy/tests runs every template, with this repo's own
#      Constraints, against passing and failing cases. Any failed case fails this script.
#   2. gator test, with --render or a render directory: the manifests deploy/validate.sh renders,
#      with workloads expanded into the Pods they would create (tests/expansion.yaml), against the
#      real Constraints, skipping the namespaces deploy/gatekeeper/config/config.yaml exempts.
#      That is what audit would report on the cluster, minus what Istio injects at runtime.
#      Every Constraint is dryrun, so findings are listed, not a failure.
# Needs bash, awk, sort, and gator on PATH or docker (pinned image below); --render also needs
# what deploy/validate.sh needs.
# Usage: deploy/policy/check.sh [--render | RENDER_DIR]
#   deploy/policy/check.sh --render      runs deploy/validate.sh into a temporary directory first
#   RENDER_DIR=/tmp/render deploy/validate.sh && deploy/policy/check.sh /tmp/render
set -euo pipefail

cd "$(dirname "$0")/../.."

GATOR_IMAGE="openpolicyagent/gator:v3.23.1@sha256:63462f0ab3d207b4bf0872f771d6e63fed5664b8837201aeb87d4ee3a5c9d754"
# Gatekeeper runs with its CEL engine off (deploy/gatekeeper/values.yaml); gator must match.
engine=(--enable-k8s-native-validation=false)
render="${1:-}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

gator() {
  if type -P gator >/dev/null; then
    command gator "$@"
  else
    # Same paths inside the container as outside, so file arguments need no translation.
    docker run --rm -v "$PWD:$PWD:ro" -v "$work:$work:ro" -w "$PWD" "$GATOR_IMAGE" "$@"
  fi
}

echo "--- gator verify deploy/policy/tests"
gator verify "${engine[@]}" deploy/policy/tests

[ -n "$render" ] || exit 0
if [ "$render" = "--render" ]; then
  render="$work/render"
  RENDER_DIR="$render" deploy/validate.sh
fi
render="$(cd "$render" && pwd)"

# Gatekeeper's Config exempts namespaces cluster-wide; gator test ignores Config, so copy the same
# list into each Constraint's match.
exempt="$(awk '
  /excludedNamespaces:/ { inlist = 1; next }
  inlist && /^[[:space:]]*#/ { next }
  inlist && /^[[:space:]]*- / { printf "%s\"%s\"", sep, $2; sep = ", "; next }
  inlist { exit }
' deploy/gatekeeper/config/config.yaml)"
[ -n "$exempt" ] || { echo "no excludedNamespaces found in config.yaml" >&2; exit 1; }

mkdir -p "$work/policy" "$work/manifests"
cp deploy/policy/templates/k8s*.yaml deploy/policy/tests/expansion.yaml "$work/policy/"
for file in deploy/policy/constraints/*.yaml; do
  awk -v list="$exempt" '{ print } /^  match:$/ { print "    excludedNamespaces: [" list "]" }' \
    "$file" >"$work/policy/$(basename "$file")"
done
cp -R "$render/." "$work/manifests/" # keep subdirectories: apps/istiod.yaml is not istiod.yaml
# validate.sh renders the templates too; they are already in $work/policy.
rm -f "$work/manifests/kustomize-policy-templates.yaml"

echo "--- gator test $render (exempt: $exempt)"
gator test "${engine[@]}" --filename "$work/policy" --filename "$work/manifests" | tee "$work/findings.txt"

echo "--- findings per constraint"
awk -F'[][]' '/\["pr-/ { n = split($2, c, ","); for (i = 1; i <= n; i++) { gsub(/[" ]/, "", c[i]); print c[i] } }' \
  "$work/findings.txt" | sort | uniq -c
