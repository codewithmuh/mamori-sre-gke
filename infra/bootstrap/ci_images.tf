# ci-images: the identity .github/workflows/images.yml uses to push the lab's container images to
# Artifact Registry. It does not sign: signatures are keyless (Sigstore, from GitHub's own OIDC
# token), so no Google key or role is involved in signing.

resource "google_service_account" "ci_images" {
  account_id   = "ci-images"
  display_name = "Image publisher (GitHub Actions, main only)"
  description  = "Pushes images built from main to the Artifact Registry repository mamori, and nothing else."

  depends_on = [google_project_service.enabled]
}

# Only jobs that run on the main branch of this repository, in no GitHub environment. GitHub's sub
# claim for such a job is "<subject prefix>:ref:refs/heads/main"; pull requests
# ("...:pull_request"), other branches, tags and environment jobs ("...:environment:<name>") get a
# different sub, so they can't use this account. The pool itself only accepts this repository.
resource "google_service_account_iam_member" "ci_images_wif" {
  service_account_id = google_service_account.ci_images.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principal://iam.googleapis.com/${local.pool}/subject/${var.github_subject_prefix}:ref:refs/heads/main"
}

# Write access to Artifact Registry is deliberately not granted in this root. It is a binding on
# the repository itself, in infra/envs/lab/iam.tf next to gke_nodes_pull (requested in #28):
#
# - The repository mamori is created by the lab root and deleted by `make down`. This root is
#   applied once, before the lab exists, so it can't bind to the repository.
# - A project-level grant limited by an IAM condition on resource.name grants nothing: Artifact
#   Registry is not among the services that pass resource attributes to IAM Conditions
#   (https://cloud.google.com/iam/docs/conditions-resource-attributes), so the condition never
#   matches.
# - A project-level grant without a condition would also reach the spend-guard repository, which
#   Cloud Run functions builds and deploys the spend-guard function from.
# - Resource Manager tags (a tag value bound to the repository and resource.matchTag() in the
#   condition) would work, but need a tag key and value here, Tag User for tf-apply and a tag
#   binding in the lab root anyway: more parts for the same scope.
#
# A repository-level binding is exactly one repository wide and lives and dies with it. tf-apply
# can already set repository IAM (roles/artifactregistry.admin), and it is not a project-level
# grant, so local.lab_grantable_roles stays as it is.

output "ci_images_sa" {
  description = "Email of the service account that pushes images from main (images.yml); the lab root grants it Artifact Registry Writer on the mamori repository."
  value       = google_service_account.ci_images.email
}
