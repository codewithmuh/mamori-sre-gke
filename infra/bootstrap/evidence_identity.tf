# Identity of the weekly evidence workflow (.github/workflows/evidence.yml): it reads cluster
# versions, nodes, Gatekeeper and Argo CD status, and SLO status for the compliance evidence bundle
# (docs/compliance/evidence-bundle.md). It can change nothing. The workflow's Terraform drift check
# uses tf-plan, not this account.
#
# Why its own workload identity pool instead of the shared pool `github`: in Workload Identity
# Federation a principal belongs to a pool, not to a provider, and the shared pool accepts every
# workflow of the repository, on any branch or pull request. Here the provider itself refuses every
# token except one from the evidence workflow on main, started by its schedule or by hand. No other
# workflow, branch or event can get a federated token from this pool at all, so the binding below can
# trust the whole pool. The rule sits in one condition and doesn't depend on attribute mappings of
# the shared provider. Pools, providers, service accounts and custom roles cost nothing and, like
# everything in bootstrap, stay when `make down` destroys the lab (scripts/leftover-check.sh doesn't
# need to know about them).
#
# Apply order: bootstrap (this file) first; the evidence workflow needs no new repository variable
# (it derives this pool's provider and the account's email from GCP_WIF_PROVIDER and GCP_PROJECT_ID).
# Until this is applied, its cloud steps record "not collected" instead of failing.

locals {
  # GitHub's workflow_ref claim: <owner>/<repo>/<path>@<ref>. Not affected by the immutable
  # subject format, which changes only the sub claim.
  evidence_workflow_ref = "${var.github_repository}/.github/workflows/evidence.yml@refs/heads/main"

  evidence_kube_role_id = "evidenceKubeReader"

  evidence_reader_roles = {
    # clusters.get and clusters.list: versions and node pools from the GKE API, and the
    # container.clusters.get that GKE requires before it authorises any Kubernetes API request.
    "roles/container.clusterViewer" = "read cluster and node pool versions; required to talk to the cluster at all"
    # Services, SLOs and the select_slo_* time series. Telemetry only: no secrets, no logs.
    "roles/monitoring.viewer" = "read SLOs, compliance and error budget from Cloud Monitoring"
    # Instead of roles/container.viewer, which can read every ConfigMap, Role and Pod spec.
    "projects/${var.project_id}/roles/${local.evidence_kube_role_id}" = "list nodes, Gatekeeper constraints and Argo CD applications"
  }
}

resource "google_iam_workload_identity_pool" "evidence" {
  workload_identity_pool_id = "github-evidence"
  display_name              = "GitHub evidence workflow"
  description               = "Only .github/workflows/evidence.yml on main of ${var.github_repository}"

  depends_on = [google_project_service.enabled]
}

resource "google_iam_workload_identity_pool_provider" "evidence" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.evidence.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-actions"
  display_name                       = "GitHub Actions OIDC (evidence)"

  # Every clause must hold. repository and repository_id: this repository and not a re-created one
  # with the same name. ref and workflow_ref: the workflow file as it is on main, so a branch can't
  # bring its own copy. event_name: the weekly schedule or a manual run; not pull_request_target,
  # workflow_run or any other event that runs main's code for someone else's trigger.
  attribute_condition = join(" && ", [
    "assertion.repository == \"${var.github_repository}\"",
    "assertion.repository_id == \"${var.github_repository_id}\"",
    "assertion.ref == \"refs/heads/main\"",
    "assertion.workflow_ref == \"${local.evidence_workflow_ref}\"",
    "assertion.event_name in [\"schedule\", \"workflow_dispatch\"]",
  ])

  attribute_mapping = {
    # repo:<owner>@<owner-id>/<repo>@<repo-id>:ref:refs/heads/main; shown as the principal in Cloud
    # Audit Logs.
    "google.subject"         = "assertion.sub"
    "attribute.workflow_ref" = "assertion.workflow_ref"
    "attribute.event_name"   = "assertion.event_name"
  }

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account" "evidence_reader" {
  account_id   = "evidence-reader"
  display_name = "Evidence reader (GitHub Actions, read-only)"
  description  = "Weekly evidence workflow on main: reads cluster versions, nodes, Gatekeeper and Argo CD status, and SLOs. Writes nothing."

  depends_on = [google_project_service.enabled]
}

# Anything that got a token from the evidence pool passed the provider's condition above.
resource "google_service_account_iam_member" "evidence_reader_wif" {
  service_account_id = google_service_account.evidence_reader.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.evidence.name}/*"
}

# GKE checks Kubernetes RBAC first and then IAM, so these IAM permissions are enough to list the
# objects without a ClusterRole in the cluster. thirdPartyObjects covers custom resources: Gatekeeper
# constraints (kubectl get constraints) and Argo CD Applications. Secrets aren't custom resources.
resource "google_project_iam_custom_role" "evidence_kube_reader" {
  project     = var.project_id
  role_id     = local.evidence_kube_role_id
  title       = "Evidence reader: nodes and custom resources"
  description = "List nodes and custom resources (Gatekeeper constraints, Argo CD applications) for the evidence bundle."
  permissions = [
    "container.nodes.list",
    "container.thirdPartyObjects.list",
  ]

  depends_on = [google_project_service.enabled]
}

# GKE has no per-cluster IAM, so these are granted on the project; the lab has one cluster.
resource "google_project_iam_member" "evidence_reader" {
  for_each = local.evidence_reader_roles

  project = var.project_id
  role    = each.key
  member  = google_service_account.evidence_reader.member

  depends_on = [google_project_iam_custom_role.evidence_kube_reader]
}

output "evidence_reader_sa" {
  description = "Email of the read-only account of the evidence workflow (evidence-reader@<project>.iam.gserviceaccount.com; the workflow derives it)."
  value       = google_service_account.evidence_reader.email
}

output "evidence_wif_provider" {
  description = "Full resource name of the provider that only the evidence workflow on main can use (the workflow derives it from GCP_WIF_PROVIDER)."
  value       = google_iam_workload_identity_pool_provider.evidence.name
}
