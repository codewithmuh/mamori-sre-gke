# Copy the first four into GitHub repository variables (see infra/README.md):
# GCP_WIF_PROVIDER, GCP_TF_PLAN_SA, GCP_TF_APPLY_SA, TF_STATE_BUCKET.

output "wif_provider" {
  description = "Full resource name of the Workload Identity Federation provider, for google-github-actions/auth."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "tf_plan_sa" {
  description = "Email of the read-only service account that plans from any branch or pull request."
  value       = google_service_account.tf_plan.email
}

output "tf_apply_sa" {
  description = "Email of the service account that applies, usable only from the GitHub environment lab."
  value       = google_service_account.tf_apply.email
}

output "state_bucket" {
  description = "GCS bucket that holds the state of infra/envs/lab."
  value       = google_storage_bucket.tfstate.name
}

output "spend_guard_source_bucket" {
  description = "GCS bucket that holds the zipped spend-guard source (infra/envs/lab uploads to it)."
  value       = google_storage_bucket.spend_guard_source.name
}
