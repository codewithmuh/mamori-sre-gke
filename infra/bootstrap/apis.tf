# Every API that this root and infra/envs/lab call. Enabling them here keeps serviceusage admin
# rights away from the CI identity.
locals {
  apis = toset([
    "artifactregistry.googleapis.com",     # image repos
    "billingbudgets.googleapis.com",       # the mamori-lab budget
    "certificatemanager.googleapis.com",   # frontend certificate and map
    "cloudbilling.googleapis.com",         # billing-account IAM grants below
    "cloudbuild.googleapis.com",           # builds the spend-guard function
    "cloudfunctions.googleapis.com",       # spend-guard function
    "cloudresourcemanager.googleapis.com", # project IAM
    "compute.googleapis.com",              # VPC, NAT, static IP, Cloud Armor
    "container.googleapis.com",            # GKE
    "eventarc.googleapis.com",             # Pub/Sub trigger of the function
    "iam.googleapis.com",                  # service accounts, custom role, WIF pool
    "iamcredentials.googleapis.com",       # short-lived tokens for impersonation from GitHub
    "logging.googleapis.com",              # node, workload and function logs
    "monitoring.googleapis.com",           # Managed Service for Prometheus, Cloud Monitoring
    "pubsub.googleapis.com",               # budget-alerts topic
    "run.googleapis.com",                  # Cloud Run functions run on Cloud Run
    "serviceusage.googleapis.com",         # this list itself
    "storage.googleapis.com",              # state and source buckets
    "sts.googleapis.com",                  # token exchange for Workload Identity Federation
  ])
}

resource "google_project_service" "enabled" {
  for_each = local.apis

  project = var.project_id
  service = each.value

  # Leave APIs on when this root is destroyed: turning them off can break other resources and
  # costs nothing to keep.
  disable_on_destroy         = false
  disable_dependent_services = false
}
