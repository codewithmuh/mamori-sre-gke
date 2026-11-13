# Identity of the OpenTelemetry Collector (deploy/observability): it writes traces to Cloud Trace,
# nothing else.
#
# No Google service account. Workload Identity Federation for GKE lets IAM name a Kubernetes
# service account directly as a principal, and Google's GKE docs now recommend that over linking
# to a service account; Cloud Trace supports these federated principals with no known
# limitations (IAM "Identity federation: products and limitations", checked 2026-11-13). So
# there is no key, no impersonation, no annotation on the Kubernetes service account, and
# tf-apply needs no actAs on anything new.
#
# Apply order. The principal lives in the cluster's workload identity pool
# (<project>.svc.id.goog), which Google creates with the first cluster that has Workload Identity
# on. Granting to a principal in a pool that doesn't exist yet fails ("Identity Pool does not
# exist"), so the grant waits for a flag:
#   1. make bootstrap                      (flag off: nothing here is created)
#   2. make up                             (creates the cluster, and with it the pool)
#   3. add `gke_workload_pool_exists = true` to infra/bootstrap/terraform.tfvars,
#      then terraform apply in infra/bootstrap again
# Until step 3 the collector runs but Cloud Trace answers PermissionDenied; nothing else depends
# on it. Keep the flag in terraform.tfvars (gitignored), so later bootstrap applies keep the grant.

variable "gke_workload_pool_exists" {
  description = "Set to true once the GKE cluster exists (its workload identity pool <project>.svc.id.goog is created with it). Shared by every grant to a Kubernetes service account principal (trace collector, agent)."
  type        = bool
  default     = false
}

locals {
  observability_collector_namespace = "observability"
  observability_collector_ksa       = "otel-collector"
}

# The principal identifier needs the project number. Read only when the grant is wanted, so the
# very first bootstrap apply (before the Resource Manager API is on) never calls the API for it.
data "google_project" "observability" {
  count = var.gke_workload_pool_exists ? 1 : 0

  project_id = var.project_id
}

locals {
  # principal://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<project>.svc.id.goog/subject/ns/<ns>/sa/<ksa>
  observability_collector_principal = var.gke_workload_pool_exists ? join("/", [
    "principal://iam.googleapis.com/projects/${data.google_project.observability[0].number}",
    "locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog",
    "subject/ns/${local.observability_collector_namespace}/sa/${local.observability_collector_ksa}",
  ]) : null
}

# Cloud Trace Agent: write spans (cloudtrace.traces.patch, telemetry.traces.write) and read trace
# insights; it can't list or read traces. Project level because Cloud Trace has no finer resource
# to grant on.
resource "google_project_iam_member" "observability_collector_trace_agent" {
  count = var.gke_workload_pool_exists ? 1 : 0

  project = var.project_id
  role    = "roles/cloudtrace.agent"
  member  = local.observability_collector_principal

  depends_on = [google_project_service.enabled]
}

output "observability_collector_principal" {
  description = "IAM principal of the trace collector's Kubernetes service account (observability/otel-collector), or null until gke_workload_pool_exists is true."
  value       = local.observability_collector_principal
}
