# Spend guard (D12): on every budget notification the function compares cost with the budget and, at
# THRESHOLD, turns node autoscaling off and sets the node pool size to 0. Nothing is deleted;
# `make resume` brings the nodes back. The code lives in tools/spend-guard.

locals {
  spend_guard_source_dir = "${path.module}/../../../tools/spend-guard"
}

# Zip the Go module. Tests, test data, the local runner in cmd/, lint config and docs are left
# out: the buildpack builds only the function package, and changing them shouldn't redeploy it.
# The fixed file mode makes the zip identical on every machine, so CI and a local run agree on
# its hash.
data "archive_file" "spend_guard" {
  type             = "zip"
  source_dir       = local.spend_guard_source_dir
  output_path      = "${path.module}/.build/spend-guard.zip"
  output_file_mode = "0666"
  excludes = [
    "**/*_test.go",
    "**/testdata/**",
    "cmd/**",
    "**/*.md",
    ".golangci.yml",
    "coverage.out",
  ]
}

# The hash in the name makes a code change produce a new object, which redeploys the function.
resource "google_storage_bucket_object" "spend_guard_source" {
  name         = "spend-guard-${data.archive_file.spend_guard.output_md5}.zip"
  bucket       = local.spend_guard_source_bucket
  source       = data.archive_file.spend_guard.output_path
  content_type = "application/zip"
}

# Built images go to a repository Terraform owns, so `make down` removes them too (the default
# gcf-artifacts repository would be left behind) and the build account can push only here.
resource "google_artifact_registry_repository" "spend_guard" {
  repository_id = "spend-guard"
  location      = local.region
  format        = "DOCKER"
  description   = "Images built by Cloud Run functions for spend-guard."

  cleanup_policy_dry_run = false

  cleanup_policies {
    id     = "keep-3-newest"
    action = "KEEP"
    most_recent_versions {
      keep_count = 3
    }
  }

  cleanup_policies {
    id     = "delete-after-14d"
    action = "DELETE"
    condition {
      tag_state  = "ANY"
      older_than = "14d"
    }
  }
}

resource "google_cloudfunctions2_function" "spend_guard" {
  name        = "spend-guard"
  location    = local.region
  description = "Scales node pool ${local.node_pool_name} of cluster ${local.cluster_name} to zero when the budget is spent."

  build_config {
    # Latest GA Go runtime of Cloud Run functions (runtime support page, 2026-11-13).
    runtime     = "go127"
    entry_point = "ScaleToZero"

    service_account   = "projects/${var.project_id}/serviceAccounts/${local.spend_guard_build_sa}"
    docker_repository = google_artifact_registry_repository.spend_guard.id

    source {
      storage_source {
        bucket = local.spend_guard_source_bucket
        object = google_storage_bucket_object.spend_guard_source.name
      }
    }
  }

  service_config {
    # One instance at a time: two copies resizing the same pool would only fight.
    max_instance_count = 1
    min_instance_count = 0
    available_memory   = "256M"
    # The maximum for event-driven functions; waiting for a resize operation can take minutes.
    timeout_seconds = 540

    # Only Eventarc (Pub/Sub push) calls it.
    ingress_settings               = "ALLOW_INTERNAL_ONLY"
    all_traffic_on_latest_revision = true
    service_account_email          = local.spend_guard_sa

    environment_variables = {
      PROJECT_ID   = var.project_id
      LOCATION     = var.zone
      CLUSTER_NAME = google_container_cluster.mamori.name
      NODE_POOL    = google_container_node_pool.spot.name
      THRESHOLD    = format("%.2f", var.spend_guard_threshold)
      DRY_RUN      = tostring(var.spend_guard_dry_run)
    }
  }

  event_trigger {
    trigger_region = local.region
    event_type     = "google.cloud.pubsub.topic.v1.messagePublished"
    pubsub_topic   = google_pubsub_topic.budget_alerts.id

    # Retry transient failures (for example a node pool upgrade still running). The function
    # returns success for messages that need no action, so they are not retried.
    retry_policy = "RETRY_POLICY_RETRY"

    # Pub/Sub signs its push with this account, which may invoke the function (iam.tf).
    service_account_email = local.spend_guard_sa
  }

  depends_on = [
    # The build fails if the build account's roles haven't propagated yet. Creating the function
    # after the node pool (several minutes) gives IAM that time without a sleep resource.
    google_container_node_pool.spot,
    google_project_iam_member.spend_guard_build_logs,
    google_project_iam_member.spend_guard_build_source,
    google_artifact_registry_repository_iam_member.spend_guard_build_push,
    google_project_iam_member.spend_guard_scaler,
    google_project_iam_member.spend_guard_invoker,
  ]
}
