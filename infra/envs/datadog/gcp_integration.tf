# Datadog's Google Cloud integration, keyless: Datadog's own service account (the delegate)
# impersonates ours, which can only read. Gives Datadog the load balancer, Cloud Armor and VM
# metrics that the in-cluster Agent can't see.
#
# Unlike the lab root, this root creates a service account and project role bindings itself.
# That is deliberate: it is applied only by the maintainer with Owner credentials, never by CI,
# and everything here is removed again by `make datadog-destroy` at the end of the window.

locals {
  # The roles Datadog documents for service account impersonation (docs.datadoghq.com/integrations/
  # google-cloud-platform/, checked 2026-11-13). All read-only.
  datadog_gcp_roles = {
    "roles/monitoring.viewer" = "read Cloud Monitoring metrics"
    "roles/compute.viewer"    = "read Compute Engine resources (nodes, load balancer pieces)"
    "roles/cloudasset.viewer" = "read asset metadata"
    "roles/browser"           = "discover the project; needed in the service account's own project"
  }
}

resource "google_service_account" "datadog" {
  account_id   = "datadog-integration"
  display_name = "Datadog Google Cloud integration (14-day window)"
  description  = "Read-only. Impersonated by Datadog's delegate account; has no keys. Deleted after the window."
}

resource "google_project_iam_member" "datadog" {
  for_each = local.datadog_gcp_roles

  project = var.project_id
  role    = each.key
  member  = google_service_account.datadog.member
}

# Lets Datadog mint short-lived tokens for our account, and nothing else.
resource "google_service_account_iam_member" "datadog_impersonation" {
  service_account_id = google_service_account.datadog.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${var.datadog_delegate_email}"
}

resource "datadog_integration_gcp_sts" "lab" {
  client_email = google_service_account.datadog.email
  account_tags = local.datadog_tags

  # Expected VM shutdowns (Spot preemptions, upgrades) mute monitors scoped to that VM.
  automute = true

  # Metrics only. Asset scans, posture management and change tracking aren't needed for alert
  # design, and each adds API traffic and data to look after.
  resource_collection_enabled           = false
  is_cspm_enabled                       = false
  is_security_command_center_enabled    = false
  is_resource_change_collection_enabled = false

  # The global external Application Load Balancer reports its metrics with location "global".
  is_global_location_enabled = true

  # Cloud Monitoring bills reads by time series returned ($0.50 per million after the first
  # million per billing account a month, since 2025-10-02). The namespace id "gke" follows
  # Datadog's metric prefix gcp.gke.*; the Datadog docs only show "pubsub" as an example id. If
  # the API rejects it, apply with gcp_disabled_metric_namespaces = [].
  metric_namespace_configs = [
    for ns in var.gcp_disabled_metric_namespaces : { id = ns, disabled = true, filters = [] }
  ]

  # Datadog may check the impersonation when the account is registered, so the grants go first.
  depends_on = [
    google_project_iam_member.datadog,
    google_service_account_iam_member.datadog_impersonation,
  ]
}
