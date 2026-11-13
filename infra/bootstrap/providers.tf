provider "google" {
  project = var.project_id
  region  = var.region

  default_labels = local.labels

  # The maintainer applies this root with user credentials (gcloud ADC). Some APIs used here, such as
  # Cloud Billing, reject user credentials unless the request names a quota project, so send this
  # project as the quota project on every call.
  user_project_override = true
  billing_project       = var.project_id
}
