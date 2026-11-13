# Datadog for the planned 14-day window (D3): synthetics, monitors, the Google Cloud integration,
# and the SLOs that tools/slo-gen writes into slo_generated.tf.json next to these files.
#
# Applied by the maintainer from a laptop (`make datadog-apply`), never by CI: it needs Datadog
# keys, and it creates a Google service account and grants it roles, which tf-apply can't do.
# Plan, timeline and teardown: docs/datadog-window.md.
terraform {
  # Local runs use 1.9.8 and CI uses 1.17.x; both must work.
  required_version = ">= 1.9.0, < 2.0.0"

  # Partial configuration, completed at init time (the same bucket as the lab, its own prefix):
  #   terraform init -backend-config="bucket=$TF_STATE_BUCKET" -backend-config="prefix=datadog"
  backend "gcs" {}

  required_providers {
    datadog = {
      source  = "DataDog/datadog"
      version = "4.27.0"
    }
    google = {
      source  = "hashicorp/google"
      version = "8.9.0"
    }
  }
}
