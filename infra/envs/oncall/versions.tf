terraform {
  # Local runs use 1.9.8 and CI uses 1.17.x; both must work.
  required_version = ">= 1.9.0, < 2.0.0"

  # Same state bucket as infra/envs/lab, its own prefix. Partial configuration, completed at init:
  #   terraform init -backend-config="bucket=$TF_STATE_BUCKET" -backend-config="prefix=oncall"
  backend "gcs" {}

  required_providers {
    pagerduty = {
      source  = "PagerDuty/pagerduty"
      version = "3.37.0"
    }
  }
}
