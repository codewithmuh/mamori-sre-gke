locals {
  labels = {
    app        = "mamori"
    env        = "lab"
    managed-by = "terraform"
  }

  state_bucket              = "${var.project_id}-tfstate"
  spend_guard_source_bucket = "${var.project_id}-spend-guard-src"

  # infra/envs/lab refers to these by name. Keep both roots in step if one changes.
  spend_guard_role_id = "spendGuardNodePoolScaler"

  # The only project-level roles tf-apply may grant or revoke (IAM allows at most 10 here). These are
  # exactly the roles infra/envs/lab/iam.tf binds. A new project-level grant in the lab root needs a
  # line here and a bootstrap apply by the maintainer first; that is the point.
  lab_grantable_roles = [
    "roles/container.defaultNodeServiceAccount",
    "roles/logging.logWriter",
    "roles/run.invoker",
    "roles/storage.objectViewer",
    "projects/${var.project_id}/roles/${local.spend_guard_role_id}",
  ]
}
