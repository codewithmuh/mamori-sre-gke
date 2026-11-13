# Role bindings for the accounts infra/bootstrap creates. tf-apply may grant only the project
# roles listed in infra/bootstrap/locals.tf (lab_grantable_roles); keep the two in step.

# GKE nodes: the predefined node role (write logs and metrics, autoscaler metrics) and nothing else.
resource "google_project_iam_member" "gke_nodes" {
  project = var.project_id
  role    = "roles/container.defaultNodeServiceAccount"
  member  = "serviceAccount:${local.gke_nodes_sa}"
}

# Read access to the image repository mamori is granted in infra/bootstrap/registry.tf, next to
# the repository.

# Spend guard: the custom role from infra/bootstrap (get the cluster and node pool, set size and
# autoscaling, read the operation). GKE has no per-cluster IAM, so it is granted on the project.
resource "google_project_iam_member" "spend_guard_scaler" {
  project = var.project_id
  role    = local.spend_guard_role
  member  = "serviceAccount:${local.spend_guard_sa}"
}

# The Pub/Sub push to a Cloud Run function is an authenticated call as the trigger account. Granted
# on the project because setting IAM on the function's Cloud Run service would need Cloud Run
# Admin for tf-apply; spend-guard is the only Cloud Run service in the project.
resource "google_project_iam_member" "spend_guard_invoker" {
  project = var.project_id
  role    = "roles/run.invoker"
  member  = "serviceAccount:${local.spend_guard_sa}"
}

# Build account of the function (instead of the default compute account): write build logs ...
resource "google_project_iam_member" "spend_guard_build_logs" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${local.spend_guard_build_sa}"
}

# ... read the source zip, from our bucket or from the copy Cloud Run functions keeps in its own
# gcf-v2-sources-<project number>-<region> bucket, and no other bucket (not the state bucket) ...
resource "google_project_iam_member" "spend_guard_build_source" {
  project = var.project_id
  role    = "roles/storage.objectViewer"
  member  = "serviceAccount:${local.spend_guard_build_sa}"

  condition {
    title       = "spend-guard-source-buckets"
    description = "Only the buckets that hold spend-guard source."
    expression  = "resource.name.startsWith('projects/_/buckets/${local.spend_guard_source_bucket}') || resource.name.startsWith('projects/_/buckets/gcf-v2-sources-')"
  }
}

# ... and push the built image to its own repository only.
resource "google_artifact_registry_repository_iam_member" "spend_guard_build_push" {
  project    = var.project_id
  location   = google_artifact_registry_repository.spend_guard.location
  repository = google_artifact_registry_repository.spend_guard.name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${local.spend_guard_build_sa}"
}
