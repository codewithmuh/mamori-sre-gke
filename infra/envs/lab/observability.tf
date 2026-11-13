# Cloud Logging pieces of the observability setup (docs/observability.md):
#   - a log-based metric that counts Spot preemptions of the cluster's nodes (no free system
#     metric says "Compute Engine took this VM back"), shown on the platform dashboard;
#   - an exclusion that keeps only a sample of the shop's routine INFO/DEBUG lines, the biggest
#     and least useful part of the log volume.
#
# tf-apply needs roles/logging.configWriter for both (requested from infra/bootstrap by #19).

# ---------------------------------------------------------------------------------------------
# Spot preemptions
# ---------------------------------------------------------------------------------------------

# Compute Engine writes a system-event audit log entry, method compute.instances.preempted, when it
# reclaims a Spot VM. Those entries land in the _Required bucket, which costs nothing and can't be
# excluded. A preemption can also show up as compute.instances.delete by system@google.com without
# the preempted entry (Compute Engine troubleshooting docs), so the dashboard shows GKE's own
# kubernetes.io/node/interruption_count next to this metric.
resource "google_logging_metric" "observability_spot_preemptions" {
  name        = "spot_preemptions"
  description = "Spot VMs of GKE cluster ${local.cluster_name} preempted by Compute Engine (system event compute.instances.preempted)."

  filter = join(" AND ", [
    "resource.type=\"gce_instance\"",
    "logName=\"projects/${var.project_id}/logs/cloudaudit.googleapis.com%2Fsystem_event\"",
    "protoPayload.methodName=\"compute.instances.preempted\"",
    # GKE names node VMs gke-<cluster>-<pool>-<hash>-<suffix>.
    "protoPayload.resourceName:\"/instances/gke-${local.cluster_name}-\"",
  ])

  metric_descriptor {
    metric_kind  = "DELTA"
    value_type   = "INT64"
    unit         = "1"
    display_name = "Spot preemptions (${local.cluster_name})"

    labels {
      key         = "node"
      value_type  = "STRING"
      description = "Name of the preempted VM, which is also the Kubernetes node name."
    }
  }

  label_extractors = {
    node = "REGEXP_EXTRACT(protoPayload.resourceName, \"/instances/([^/]+)$\")"
  }
}

# ---------------------------------------------------------------------------------------------
# Log volume: sample the shop's routine lines
# ---------------------------------------------------------------------------------------------

variable "observability_shop_info_log_keep_percent" {
  description = "Share (0-100) of the shop's INFO and DEBUG container log lines (namespace boutique) kept in Cloud Logging. WARNING and above, and every line with no or an unknown severity, are always kept. 100 turns the exclusion off, for example during a game day."
  type        = number
  default     = 10

  validation {
    condition     = var.observability_shop_info_log_keep_percent >= 0 && var.observability_shop_info_log_keep_percent <= 100
    error_message = "observability_shop_info_log_keep_percent must be between 0 and 100."
  }
}

# Why: Online Boutique logs a line or two per request in most services (currency conversions,
# cart reads, shipping quotes, the frontend's request log) and the load generator prints its
# stats table every few seconds. Under the steady load that is about 30 lines a second, roughly
# 45-75 GiB a month (estimate, docs/observability.md), against Cloud Logging's free 50 GiB a month
# per project; above it, $0.50 per GiB. Keeping 10 % of INFO/DEBUG brings the shop down to about
# 5-8 GiB and leaves room for everything else.
#
# What is kept: every WARNING, ERROR and CRITICAL line (the ones game days, postmortems and the
# incident agent look for), every line whose severity is missing or unparsed (DEFAULT), all other
# namespaces, and all audit and system logs. sample(insertId, f) matches a fraction f of entries,
# and matched entries are excluded, so f is the share dropped.
resource "google_logging_project_exclusion" "observability_shop_info_logs" {
  count = var.observability_shop_info_log_keep_percent < 100 ? 1 : 0

  name        = "shop-info-logs-sampled"
  description = "Drop ${100 - var.observability_shop_info_log_keep_percent} % of INFO/DEBUG container logs of namespace boutique on cluster ${local.cluster_name} (infra/envs/lab/observability.tf)."

  filter = join(" AND ", [
    "resource.type=\"k8s_container\"",
    "resource.labels.cluster_name=\"${local.cluster_name}\"",
    "resource.labels.namespace_name=\"boutique\"",
    "(severity=\"DEBUG\" OR severity=\"INFO\")",
    "sample(insertId, ${(100 - var.observability_shop_info_log_keep_percent) / 100})",
  ])
}
