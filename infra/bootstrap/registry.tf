# Images of the forked services and SRE tools. The repository lives here, not in infra/envs/lab, so
# images survive `make down`: `make up` deploys the same images again without a rebuild, and the
# push grant for images.yml (ci_images.tf) binds straight to it.
resource "google_artifact_registry_repository" "mamori" {
  repository_id = "mamori"
  location      = var.region
  format        = "DOCKER"
  description   = "Images of the mamori lab (forked Online Boutique services, SRE tools)."

  # A tag, once pushed, always means the same image: deploy/ pins version tags, and nobody can
  # repoint one at different content. images.yml pushes only new tags (:<git sha>, :<version>).
  docker_config {
    immutable_tags = true
  }

  cleanup_policy_dry_run = false

  # KEEP rules win over DELETE rules.
  # Releases (X.Y.Z tags) stay forever: deploy/ may still run one. Git SHA tags contain no dot.
  cleanup_policies {
    id     = "keep-releases"
    action = "KEEP"
    condition {
      tag_state    = "TAGGED"
      tag_prefixes = ["0.", "1.", "2."]
    }
  }

  # The 10 newest versions of each image stay, however old.
  cleanup_policies {
    id     = "keep-10-newest"
    action = "KEEP"
    most_recent_versions {
      keep_count = 10
    }
  }

  # Older main builds (:<git sha>) go after 30 days. Only tagged versions: cosign signatures and
  # SBOM attestations are untagged artifacts, so a rule on untagged versions could delete the
  # signature of a release that is still deployed. With immutable tags no image loses its tag, so
  # there are no orphaned untagged images to clean up.
  cleanup_policies {
    id     = "delete-old-builds"
    action = "DELETE"
    condition {
      tag_state  = "TAGGED"
      older_than = "30d"
    }
  }

  depends_on = [google_project_service.enabled]
}

# GKE nodes pull the lab's images from this repository only.
resource "google_artifact_registry_repository_iam_member" "gke_nodes_pull" {
  project    = google_artifact_registry_repository.mamori.project
  location   = google_artifact_registry_repository.mamori.location
  repository = google_artifact_registry_repository.mamori.name
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.gke_nodes.member
}
