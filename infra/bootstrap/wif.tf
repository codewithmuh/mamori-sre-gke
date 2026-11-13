# Keyless CI: GitHub Actions exchanges its OIDC token for a short-lived Google token. No service
# account keys exist anywhere.
resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"
  description               = "Workflows of ${var.github_repository}"

  depends_on = [google_project_service.enabled]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-actions"
  display_name                       = "GitHub Actions OIDC"

  # GitHub uses one issuer for every repository on github.com, so without this condition any
  # repository could get a token from this pool.
  # The repository id also has to match, so a deleted and re-created repository with the same name
  # can't use the pool.
  attribute_condition = "assertion.repository == \"${var.github_repository}\" && assertion.repository_id == \"${var.github_repository_id}\""

  attribute_mapping = {
    # This repository uses GitHub's immutable subject format (default for repositories created after
    # 2026-07-15): sub is "repo:<owner>@<owner-id>/<repo>@<repo-id>:environment:<name>" for a job in a
    # GitHub environment, and "...:ref:<ref>" for a job without one.
    "google.subject"                = "assertion.sub"
    "attribute.repository"          = "assertion.repository"
    "attribute.repository_id"       = "assertion.repository_id"
    "attribute.repository_owner_id" = "assertion.repository_owner_id"
    "attribute.ref"                 = "assertion.ref"

    # "true" only for a job that declares the apply environment AND runs on main. tf-apply is bound
    # to this attribute (iam.tf). The environment alone isn't enough: GitHub creates an
    # environment, unprotected, the first time any branch's workflow names it. Built from sub and
    # ref, which every GitHub token carries; the environment claim is missing on jobs without one,
    # and a mapping that fails would break token exchange for every other job.
    "attribute.lab_main" = "(assertion.sub == '${var.github_subject_prefix}:environment:${var.github_apply_environment}' && assertion.ref == 'refs/heads/main') ? 'true' : 'false'"
  }

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}
