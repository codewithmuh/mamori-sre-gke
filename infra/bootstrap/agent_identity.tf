# Google Cloud roles of the incident agent (#24, docs/agent.md): the Kubernetes service account
# `agent` in namespace `mamori-agent`, through Workload Identity Federation for GKE. The roles go
# to that service account's principal directly, so there is no Google service account, no key
# and no impersonation. Its one Pub/Sub grant is on its own subscription, in
# infra/envs/lab/agent.tf.
#
# Everything here reads, except the model call. Nothing can write to the project.
#
# Apply order. IAM refuses principals of a workload identity pool that doesn't exist yet, and the
# pool PROJECT_ID.svc.id.goog appears with the first cluster that has Workload Identity on. The
# grants below are gated on var.gke_workload_pool_exists, which observability_identity.tf (#19)
# declares for every grant to a Kubernetes service account principal:
#   1. make bootstrap                  (false: only the custom role is created)
#   2. make up                         (creates the cluster, and with it the pool)
#   3. TF_VAR_gke_workload_pool_exists=true make bootstrap, and keep it true from then on
#      (a later run with false removes the grants again). Whether the pool survives `make down`
#      isn't documented; if a rebuilt lab's bootstrap apply fails on these grants, run `make up` first.

# Read only once the grants are wanted: on the very first bootstrap the Resource Manager API may
# not be enabled yet, and nothing else here needs the project number.
data "google_project" "agent_identity" {
  count = var.gke_workload_pool_exists ? 1 : 0

  project_id = var.project_id
}

locals {
  agent_namespace = "mamori-agent"
  agent_ksa       = "agent"
  agent_ksa_principal = var.gke_workload_pool_exists ? join("/", [
    "principal://iam.googleapis.com/projects/${data.google_project.agent_identity[0].number}",
    "locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog",
    "subject/ns/${local.agent_namespace}/sa/${local.agent_ksa}",
  ]) : null

  agent_model_role_id = "mamoriAgentModelCaller"

  agent_project_roles = {
    # Claude on Vertex AI. Anthropic's and Google's docs name the permission
    # aiplatform.endpoints.predict, "such as roles/aiplatform.user"; that role can also create
    # datasets, endpoints and training jobs, so the agent gets a custom role with the one
    # permission instead (rawPredict and streamRawPredict). Swap in roles/aiplatform.user here
    # if a call ever needs more.
    "projects/${var.project_id}/roles/${local.agent_model_role_id}" = "call Claude models on Vertex AI"
    # PromQL on Managed Service for Prometheus checks monitoring.timeSeries.list.
    "roles/monitoring.viewer" = "PromQL API and Cloud Monitoring reads"
    # entries:list on the project's logs (not data access audit logs, which need privateLogViewer).
    "roles/logging.viewer" = "Cloud Logging reads"
    # The only predefined Cloud Trace role with read access and no write access.
    "roles/cloudtrace.user" = "Cloud Trace reads"
  }
}

# Defined here, not in the lab root, so tf-apply (which can't edit roles) can't widen it.
resource "google_project_iam_custom_role" "agent_model_caller" {
  project     = var.project_id
  role_id     = local.agent_model_role_id
  title       = "mamori agent model caller"
  description = "Call publisher models on Vertex AI (rawPredict, streamRawPredict); nothing else."
  permissions = ["aiplatform.endpoints.predict"]

  depends_on = [google_project_service.enabled]
}

resource "google_project_iam_member" "agent" {
  for_each = var.gke_workload_pool_exists ? local.agent_project_roles : {}

  project = var.project_id
  role    = each.key
  member  = local.agent_ksa_principal

  depends_on = [google_project_iam_custom_role.agent_model_caller]
}
