#!/usr/bin/env bash
# Regenerate the JSON schemas kubeconform uses for the Chaos Mesh kinds in chaos/faults/manifests.
# The datreeio CRDs-catalog that deploy/validate.sh uses has no chaos-mesh.org schemas, so they
# come from the CRDs of the pinned chart, converted with kubeconform's own openapi2jsonschema.py.
# Run it after changing the chart version in deploy/apps/chaos-mesh.yaml.
# Needs helm and docker. Usage: chaos/schemas/generate.sh
set -euo pipefail

cd "$(dirname "$0")/../.."

app=deploy/apps/chaos-mesh.yaml
version="$(awk '$1 == "targetRevision:" { print $2; exit }' "$app")"
# kubeconform v0.8.0, the version deploy/validate.sh runs.
script_url="https://raw.githubusercontent.com/yannh/kubeconform/02374e583d700721f57300fae78e11acd27ee539/scripts/openapi2jsonschema.py"
uv_image="ghcr.io/astral-sh/uv:0.13.0-python3.13-trixie-slim@sha256:40215b626e22e4ff65839863624c4b885b8829b1a66ce895b7be20eca7fabe6d"
kinds=(podchaos networkchaos stresschaos)

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
helm pull chaos-mesh --repo https://charts.chaos-mesh.org --version "$version" --untar --untardir "$work"
curl -fsSL -o "$work/openapi2jsonschema.py" "$script_url"
for k in "${kinds[@]}"; do
  cp "$work/chaos-mesh/crds/chaos-mesh.org_${k}.yaml" "$work/${k}.yaml"
done

out=chaos/schemas/chaos-mesh.org
mkdir -p "$out"
# DENY_ROOT_ADDITIONAL_PROPERTIES: an unknown top-level field fails too, as with -strict.
docker run --rm -e DENY_ROOT_ADDITIONAL_PROPERTIES=1 -v "$work":/work -w /work "$uv_image" \
  uv run --no-project --with pyyaml==6.0.3 python openapi2jsonschema.py "${kinds[@]/%/.yaml}" >/dev/null
for k in "${kinds[@]}"; do
  cp "$work/${k}_v1alpha1.json" "$out/"
done
echo "chaos-mesh.org schemas from chart $version written to $out"
