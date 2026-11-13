#!/usr/bin/env bash
# Terraform checks, the same in CI and in `make lint`: format check over infra/, then for each
# root module init without a backend, validate, and tflint with infra/.tflint.hcl.
# Offline apart from downloading providers and tflint plugins; touches no cloud resources.
# Skips cleanly while infra/ has no Terraform code.
set -euo pipefail

roots=(infra/bootstrap infra/envs/lab)
config="${PWD}/infra/.tflint.hcl" # absolute, so it resolves the same under --chdir

if [[ ! -d infra ]] || [[ -z $(find infra -name '*.tf' -print -quit) ]]; then
  echo "No Terraform code under infra/ yet; skipping."
  exit 0
fi
for tool in terraform tflint; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "error: ${tool} is not installed (the dev container has it)" >&2
    exit 1
  fi
done
if [[ ! -f $config ]]; then
  echo "error: ${config} is missing; tflint needs it" >&2
  exit 1
fi

echo "terraform fmt -check (infra/)"
terraform fmt -check -diff -recursive infra

tflint --init --config="$config"

for root in "${roots[@]}"; do
  if [[ -z $(find "$root" -maxdepth 1 -name '*.tf' -print -quit 2>/dev/null) ]]; then
    echo "No Terraform code in ${root} yet; skipping it."
    continue
  fi
  echo "== ${root}"
  terraform -chdir="$root" init -backend=false -input=false -no-color >/dev/null
  terraform -chdir="$root" validate -no-color
  tflint --chdir="$root" --config="$config"
done
