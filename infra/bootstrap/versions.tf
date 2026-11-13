# Local state on purpose: this root creates the state bucket that infra/envs/lab uses, so it can't
# keep its own state there before it exists. It is applied once, by hand (see infra/README.md).
terraform {
  # Local runs use 1.9.8 and CI uses 1.17.x; both must work.
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "8.9.0"
    }
  }
}
