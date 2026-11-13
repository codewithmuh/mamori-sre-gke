locals {
  region = regex("^(.+)-[a-z]$", var.zone)[0] # asia-northeast1-a -> asia-northeast1

  labels = {
    app        = "mamori"
    env        = "lab"
    managed-by = "terraform"
  }

  cluster_name   = "mamori"
  node_pool_name = "spot-pool"
  node_tag       = "mamori-node"

  # Internal range of the GKE control plane. Must not overlap the subnet ranges in network.tf.
  control_plane_cidr = "172.16.0.0/28"

  # Fixed size in M1 (no autoscaling). `make resume` restores this after the spend guard fired.
  node_pool_size = 3

  # Created by infra/bootstrap, so that tf-apply can't create identities or edit roles. Keep the
  # names in step with infra/bootstrap/iam.tf and infra/bootstrap/locals.tf.
  gke_nodes_sa              = "gke-nodes@${var.project_id}.iam.gserviceaccount.com"
  spend_guard_sa            = "spend-guard@${var.project_id}.iam.gserviceaccount.com"
  spend_guard_build_sa      = "spend-guard-build@${var.project_id}.iam.gserviceaccount.com"
  spend_guard_role          = "projects/${var.project_id}/roles/spendGuardNodePoolScaler"
  spend_guard_source_bucket = "${var.project_id}-spend-guard-src"

  # Image repository from infra/bootstrap/registry.tf; it outlives the lab.
  artifact_registry_url = "${local.region}-docker.pkg.dev/${var.project_id}/mamori"
}
