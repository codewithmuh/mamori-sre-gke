provider "google" {
  project = var.project_id
  region  = local.region
  zone    = var.zone

  # Every resource with a top-level labels field gets these. GKE takes resource_labels instead,
  # so gke.tf sets them there too.
  default_labels = local.labels

  # The Billing Budget API rejects user credentials (a local `make up`) unless the request names a
  # quota project, so name this project on every call. Service accounts that apply or plan need
  # serviceusage.services.use on it (granted in infra/bootstrap).
  user_project_override = true
  billing_project       = var.project_id
}
