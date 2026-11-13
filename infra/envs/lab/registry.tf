# Images of the forked services. Without cleanup, every CI build adds an image and storage grows
# forever.
resource "google_artifact_registry_repository" "mamori" {
  repository_id = "mamori"
  location      = local.region
  format        = "DOCKER"
  description   = "Images of the mamori lab (forked Online Boutique services, SRE tools)."

  cleanup_policy_dry_run = false

  # KEEP rules win over DELETE rules, so the 10 newest versions of each image always stay, however
  # old they are; that covers anything still deployed.
  cleanup_policies {
    id     = "keep-10-newest"
    action = "KEEP"
    most_recent_versions {
      keep_count = 10
    }
  }

  cleanup_policies {
    id     = "delete-untagged-after-7d"
    action = "DELETE"
    condition {
      tag_state  = "UNTAGGED"
      older_than = "7d"
    }
  }

  cleanup_policies {
    id     = "delete-after-30d"
    action = "DELETE"
    condition {
      tag_state  = "ANY"
      older_than = "30d"
    }
  }
}
