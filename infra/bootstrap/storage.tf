# Remote state for infra/envs/lab. Versioned so a bad apply can be rolled back by restoring an older
# state object.
resource "google_storage_bucket" "tfstate" {
  name     = local.state_bucket
  location = var.region

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false

  versioning {
    enabled = true
  }

  # Keep the last 20 versions of each state file; older ones only cost money.
  lifecycle_rule {
    condition {
      num_newer_versions = 20
      with_state         = "ARCHIVED"
    }
    action {
      type = "Delete"
    }
  }

  # Losing this bucket loses track of everything the lab created.
  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.enabled]
}

# Holds the zipped source of the spend-guard function. It lives here, not in the lab root, so that
# tf-apply never needs storage.buckets.create (only Storage Admin has it, and that would also open
# the state bucket). A few KB of zips cost nothing while the lab is down.
resource "google_storage_bucket" "spend_guard_source" {
  name     = local.spend_guard_source_bucket
  location = var.region

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = true

  # A replaced zip stays restorable for a week.
  versioning {
    enabled = true
  }

  lifecycle_rule {
    condition {
      days_since_noncurrent_time = 7
      with_state                 = "ARCHIVED"
    }
    action {
      type = "Delete"
    }
  }

  depends_on = [google_project_service.enabled]
}
