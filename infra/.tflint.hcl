# Shared by both roots. Run from the repository root, for example:
#   tflint --init --config="$PWD/infra/.tflint.hcl"
#   tflint --config="$PWD/infra/.tflint.hcl" --chdir=infra/envs/lab
# Same tflint version as CI (v0.65.0); offline rules only (no deep check, which needs credentials).

config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

plugin "google" {
  enabled = true
  version = "0.41.0"
  source  = "github.com/terraform-linters/tflint-ruleset-google"
}

# Beyond the recommended preset: every variable and output says what it is for.
rule "terraform_documented_variables" {
  enabled = true
}

rule "terraform_documented_outputs" {
  enabled = true
}

rule "terraform_naming_convention" {
  enabled = true
}
