# Identities live here; what they may do in the lab lives in infra/envs/lab/iam.tf.
#
# The split is deliberate: the CI apply identity (tf-apply) can't create service accounts, edit
# roles or grant arbitrary roles. It can only bind the roles listed in locals.tf, and only act as
# the three service accounts the lab needs. Changing any of that takes a bootstrap apply with the
# maintainer's own credentials.

locals {
  pool = google_iam_workload_identity_pool.github.name # projects/<number>/locations/global/workloadIdentityPools/github

  # Any workflow run of the repository (any branch, any pull request from the repository itself).
  github_repo_principal = "principalSet://iam.googleapis.com/${local.pool}/attribute.repository/${var.github_repository}"

  # Only jobs that declare `environment: lab`. GitHub puts the environment into the sub claim, after
  # the repository's immutable subject prefix (see var.github_subject_prefix).
  github_apply_principal = "principal://iam.googleapis.com/${local.pool}/subject/${var.github_subject_prefix}:environment:${var.github_apply_environment}"
}

# ---------------------------------------------------------------------------------------------
# Terraform identities for GitHub Actions
# ---------------------------------------------------------------------------------------------

resource "google_service_account" "tf_plan" {
  account_id   = "tf-plan"
  display_name = "Terraform plan (GitHub Actions, read-only)"

  depends_on = [google_project_service.enabled]
}

resource "google_service_account" "tf_apply" {
  account_id   = "tf-apply"
  display_name = "Terraform apply (GitHub environment lab)"

  depends_on = [google_project_service.enabled]
}

resource "google_service_account_iam_member" "tf_plan_wif" {
  service_account_id = google_service_account.tf_plan.name
  role               = "roles/iam.workloadIdentityUser"
  member             = local.github_repo_principal
}

resource "google_service_account_iam_member" "tf_apply_wif" {
  service_account_id = google_service_account.tf_apply.name
  role               = "roles/iam.workloadIdentityUser"
  member             = local.github_apply_principal
}

# tf-plan: read everything, write nothing but lock files.
locals {
  tf_plan_project_roles = toset([
    "roles/viewer",                            # refresh every lab resource during plan
    "roles/iam.securityReviewer",              # read IAM policies on resources (e.g. the Artifact Registry repo), which viewer doesn't cover everywhere
    "roles/serviceusage.serviceUsageConsumer", # the lab provider names the project as quota project (needed by the Billing Budget API)
  ])
}

resource "google_project_iam_member" "tf_plan" {
  for_each = local.tf_plan_project_roles

  project = var.project_id
  role    = each.value
  member  = google_service_account.tf_plan.member
}

# tf-apply: one predefined role per kind of resource in infra/envs/lab, no basic roles.
locals {
  tf_apply_project_roles = {
    "roles/compute.networkAdmin"              = "VPC, subnet, router, Cloud NAT, global static IP"
    "roles/compute.securityAdmin"             = "Cloud Armor security policy (networkAdmin can't create one)"
    "roles/container.clusterAdmin"            = "GKE cluster and node pool, without access to Kubernetes objects"
    "roles/artifactregistry.admin"            = "create repositories, cleanup policies and repository-level IAM"
    "roles/certificatemanager.editor"         = "DNS authorization, managed certificate, map and map entry (includes delete)"
    "roles/pubsub.admin"                      = "budget-alerts topic; connecting a budget to a topic needs topics.setIamPolicy"
    "roles/cloudfunctions.developer"          = "spend-guard function and its Eventarc trigger"
    "roles/serviceusage.serviceUsageConsumer" = "the lab provider names the project as quota project (Billing Budget API)"
  }
}

resource "google_project_iam_member" "tf_apply" {
  for_each = local.tf_apply_project_roles

  project = var.project_id
  role    = each.key
  member  = google_service_account.tf_apply.member
}

# Project IAM Admin, but limited by an IAM condition to granting and revoking only the roles in
# local.lab_grantable_roles. Without the condition tf-apply could make itself Owner.
resource "google_project_iam_member" "tf_apply_limited_iam_admin" {
  project = var.project_id
  role    = "roles/resourcemanager.projectIamAdmin"
  member  = google_service_account.tf_apply.member

  condition {
    title       = "lab-roles-only"
    description = "May grant or revoke only the project roles that infra/envs/lab binds."
    expression  = "api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly(${jsonencode(local.lab_grantable_roles)})"
  }
}

# The Billing Budget API checks permissions on the billing account, not the project. Costs Manager
# is the narrowest predefined role that can create and delete budgets; it can't link projects,
# change payment settings or grant access. Granted so that CI can apply the whole lab root,
# including the budget. On a personal billing account that pays only for this project, the extra
# reach (all budgets and cost data of the account) is small. Remove this grant and apply
# budget.tf locally if the billing account is shared.
resource "google_billing_account_iam_member" "tf_apply_budgets" {
  billing_account_id = var.billing_account_id
  role               = "roles/billing.costsManager"
  member             = google_service_account.tf_apply.member

  depends_on = [google_project_service.enabled]
}

# Plan refreshes the budget, which needs billing.budgets.get on the billing account.
resource "google_billing_account_iam_member" "tf_plan_budgets" {
  billing_account_id = var.billing_account_id
  role               = "roles/billing.viewer"
  member             = google_service_account.tf_plan.member

  depends_on = [google_project_service.enabled]
}

# State bucket. Apply reads and writes state. Plan reads state and may create and delete only the
# lock file (<prefix>/<workspace>.tflock): any branch can run plan, so it must not be able to
# overwrite the state object.
resource "google_storage_bucket_iam_member" "tf_apply_state" {
  bucket = google_storage_bucket.tfstate.name
  role   = "roles/storage.objectUser"
  member = google_service_account.tf_apply.member
}

resource "google_storage_bucket_iam_member" "tf_plan_state_read" {
  bucket = google_storage_bucket.tfstate.name
  role   = "roles/storage.objectViewer"
  member = google_service_account.tf_plan.member
}

resource "google_storage_bucket_iam_member" "tf_plan_state_lock" {
  bucket = google_storage_bucket.tfstate.name
  role   = "roles/storage.objectUser"
  member = google_service_account.tf_plan.member

  condition {
    title       = "lock-files-only"
    description = "Create and delete Terraform lock files, nothing else."
    expression  = "resource.name.endsWith('.tflock')"
  }
}

# Spend-guard source bucket: apply uploads the zip, plan reads its metadata.
resource "google_storage_bucket_iam_member" "tf_apply_spend_guard_source" {
  bucket = google_storage_bucket.spend_guard_source.name
  role   = "roles/storage.objectUser"
  member = google_service_account.tf_apply.member
}

resource "google_storage_bucket_iam_member" "tf_plan_spend_guard_source" {
  bucket = google_storage_bucket.spend_guard_source.name
  role   = "roles/storage.objectViewer"
  member = google_service_account.tf_plan.member
}

# ---------------------------------------------------------------------------------------------
# Identities the lab's workloads run as (their roles are bound in infra/envs/lab/iam.tf)
# ---------------------------------------------------------------------------------------------

resource "google_service_account" "gke_nodes" {
  account_id   = "gke-nodes"
  display_name = "GKE nodes of cluster mamori"
  description  = "Node VMs only. Workloads use Workload Identity, not this account."

  depends_on = [google_project_service.enabled]
}

resource "google_service_account" "spend_guard" {
  account_id   = "spend-guard"
  display_name = "Spend guard function"
  description  = "Runs the spend-guard function and authenticates its Pub/Sub trigger."

  depends_on = [google_project_service.enabled]
}

resource "google_service_account" "spend_guard_build" {
  account_id   = "spend-guard-build"
  display_name = "Spend guard build"
  description  = "Cloud Build identity for the spend-guard function, instead of the default compute account."

  depends_on = [google_project_service.enabled]
}

# Exactly what the GKE API checks for the four calls the function makes:
# nodePools.get -> container.clusters.get; nodePools.setAutoscaling and nodePools.setSize ->
# container.clusters.update; operations.get -> container.operations.get. GKE has no node-pool-level
# permissions, so clusters.update can't be narrowed further. Defined here so tf-apply (which can
# grant it) can't add permissions to it.
resource "google_project_iam_custom_role" "spend_guard" {
  project     = var.project_id
  role_id     = local.spend_guard_role_id
  title       = "Spend guard node pool scaler"
  description = "Read the cluster and node pool, change node pool size and autoscaling, follow the operation."
  permissions = [
    "container.clusters.get",
    "container.clusters.update",
    "container.operations.get",
  ]

  depends_on = [google_project_service.enabled]
}

# tf-apply attaches these accounts to the node pool and the function, which needs actAs on each.
# Granted per account, not on the project, so tf-apply can't act as any other account (such as
# the default compute account).
locals {
  lab_runtime_accounts = {
    gke_nodes         = google_service_account.gke_nodes.name
    spend_guard       = google_service_account.spend_guard.name
    spend_guard_build = google_service_account.spend_guard_build.name
  }
}

resource "google_service_account_iam_member" "tf_apply_act_as" {
  for_each = local.lab_runtime_accounts

  service_account_id = each.value
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.tf_apply.member
}
