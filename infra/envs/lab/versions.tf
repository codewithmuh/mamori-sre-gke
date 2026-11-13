terraform {
  # Local runs use 1.9.8 and CI uses 1.17.x; both must work.
  required_version = ">= 1.9.0, < 2.0.0"

  # Partial configuration, completed at init time:
  #   terraform init -backend-config="bucket=$TF_STATE_BUCKET" -backend-config="prefix=lab"
  backend "gcs" {}

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "8.9.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "2.8.1"
    }
  }
}
