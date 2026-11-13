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

# Push access to the one repository the images go to, granted on the repository itself. It lives in
# this root, which survives `make down`, so the images (and this grant) outlast the lab.
#
# Narrower than any project-level grant: Artifact Registry doesn't pass resource names to IAM
# Conditions (https://cloud.google.com/iam/docs/conditions-resource-attributes), so a project grant
# could not be limited to this repository, and would also reach the spend-guard repository that
# Cloud Run functions builds and deploys the spend-guard function from.
#
# Writer can push and tag images and attach referrers (cosign signatures and attestations); it
# can't delete images or tags, or change the repository or its IAM.
resource "google_artifact_registry_repository_iam_member" "ci_images_push" {
  project    = google_artifact_registry_repository.mamori.project
  location   = google_artifact_registry_repository.mamori.location
  repository = google_artifact_registry_repository.mamori.name
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.ci_images.member
}

output "ci_images_sa" {
  description = "Email of the service account that pushes images from main (images.yml)."
  value       = google_service_account.ci_images.email
}
